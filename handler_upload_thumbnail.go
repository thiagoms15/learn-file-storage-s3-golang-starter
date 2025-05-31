package main

import (
	"fmt"
	"io"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"mime"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20 // 10 MB

	if err := r.ParseMultipartForm(maxMemory); err != nil {
		http.Error(w, "Could not parse multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, fileHeader, err := r.FormFile("thumbnail")
	if err != nil {
		http.Error(w, "Could not get thumbnail from form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	contentType := fileHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type header", err)
		return
	}

	if mediaType != "image/jpeg" && mediaType != "image/png" {
		respondWithError(w, http.StatusBadRequest, "Unsupported media type. Only image/jpeg and image/png are allowed", nil)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	}

	if video.UserID != userID {
		http.Error(w, "Unauthorized: you do not own this video", http.StatusUnauthorized)
		return
	}

	ext := getExtensionFromContentType(contentType)
	if ext == "" {
		http.Error(w, "Unsupported content type: "+contentType, http.StatusBadRequest)
		return
	}

	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to generate file name", err)
		return
	}
	randomBase64 := base64.RawURLEncoding.EncodeToString(randomBytes[:])

	filename := fmt.Sprintf("%s%s", randomBase64, ext)
	fullPath := filepath.Join(cfg.assetsRoot, filename)

	outFile, err := os.Create(fullPath)
	if err != nil {
		http.Error(w, "Failed to create file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer outFile.Close()

	if _, err := io.Copy(outFile, file); err != nil {
		http.Error(w, "Failed to save file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	url := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, filename)
	video.ThumbnailURL = &url

	if err := cfg.db.UpdateVideo(video); err != nil {
		http.Error(w, "Failed to update video metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getExtensionFromContentType(contentType string) string {
	switch strings.ToLower(contentType) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

