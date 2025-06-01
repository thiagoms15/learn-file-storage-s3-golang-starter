package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"log"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
	"encoding/base64"
	"crypto/rand"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Missing bearer token", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Invalid JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You do not own this video", nil)
		return
	}

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Could not read video file", err)
		return
	}
	defer file.Close()

	contentType := fileHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Only video/mp4 is supported", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not write temp file", err)
		return
	}

	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to rewind file", err)
		return
	}

	processedPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		log.Println("Failed to process video for fast start:", err)
		respondWithError(w, http.StatusInternalServerError, "Video processing failed", err)
		return
	}
	defer os.Remove(processedPath) // Clean up processed file

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		log.Println("warning: failed to get aspect ratio:", err)
		aspectRatio = "other"
	}

	prefix := "other/"
	if aspectRatio == "16:9" {
		prefix = "landscape/"
	} else if aspectRatio == "9:16" {
		prefix = "portrait/"
	}

	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to generate random key", err)
		return
	}
	fileName := base64.RawURLEncoding.EncodeToString(randomBytes) + ".mp4"

	s3Key := prefix + fileName

	processedFile, err := os.Open(processedPath)
	if err != nil {
		log.Println("Failed to open processed video:", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to read processed video", err)
		return
	}
	defer processedFile.Close()

	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3Key,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload to S3", err)
		return
	}

	url := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3Key)
	video.VideoURL = &url

	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

type ffprobeOutput struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	var out bytes.Buffer

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", err
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return "", err
	}

	if len(parsed.Streams) == 0 {
		return "", errors.New("no streams found in ffprobe output")
	}

	width := parsed.Streams[0].Width
	height := parsed.Streams[0].Height

	if width == 0 || height == 0 {
		return "", errors.New("invalid dimensions")
	}

	ratio := float64(width) / float64(height)

	// Integer-based classification with a small tolerance
	if width >= height {
		if abs(ratio-16.0/9.0) < 0.2 {
			return "16:9", nil
		}
	} else {
		if abs(ratio-9.0/16.0) < 0.2 {
			return "9:16", nil
		}
	}

	return "other", nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"

	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		outputPath,
	)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg faststart processing failed: %w", err)
	}

	return outputPath, nil
}
