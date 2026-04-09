package main

import (
	"fmt"
	"io"
	"net/http"

	"crypto/rand"
	"encoding/base64"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/tavis7/bootdev-tubely/internal/auth"
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

	const maxMemory = 10 << 20
	file, fileHeader, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reading from file", err)
		return
	}
	defer file.Close()

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}

	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	extension := strings.Split(mediaType, "/")[1]

	allowedFiletypes := map[string]struct{}{
		"image/jpeg": struct{}{},
		"image/png":  struct{}{},
	}

	_, ok := allowedFiletypes[mediaType]
	if !ok {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	thumbnailIDRaw := [32]byte{}
	_, err = rand.Read(thumbnailIDRaw[:])
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving file", err)
		return
	}
	thumbnailID := base64.RawURLEncoding.EncodeToString(thumbnailIDRaw[:])
	fileName := fmt.Sprintf("%v.%v", thumbnailID, extension)
	filePath := filepath.Join(cfg.assetsRoot, fileName)
	localFile, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reading from file", err)
		return
	}

	_, err = io.Copy(localFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving file", err)
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%v/assets/%v", cfg.port, fileName)

	video.ThumbnailURL = &thumbnailURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update thumbnail URL", nil)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
