package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"crypto/rand"
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/tavis7/bootdev-tubely/internal/auth"
	"github.com/tavis7/bootdev-tubely/internal/database"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("Uploading video", videoID, "by user", userID)

	video, err := cfg.db.GetVideo(videoID)
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video does not belong to user", err)
		return
	}

	const maxVideoSize = 1 << 30

	file, fileHeader, err := r.FormFile("video")
	defer file.Close()
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video", err)
		return
	}

	filetype, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", err)
		return
	}

	if filetype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest,
			"Invalid file type", fmt.Errorf("Filetype %v is not video/mp4", filetype))
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError,
			"Couldn't create temporary file", err)
		return
	}

	reader := http.MaxBytesReader(w, file, maxVideoSize)
	io.Copy(tmpFile, reader)
	tmpFile.Seek(0, io.SeekStart)

	prefix, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError,
			"Couldn't calculate aspect ratio", err)
		return
	}

	processedFilePath, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}

	processedFile, err := os.Open(processedFilePath)
	defer processedFile.Close()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed video", err)
		return
	}

	fileIDRaw := [32]byte{}
	_, err = rand.Read(fileIDRaw[:])
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving video", err)
		return
	}
	fileID := base64.RawURLEncoding.EncodeToString(fileIDRaw[:])
	filename := fmt.Sprintf("%v.mp4", fileID)
	objectKey := fmt.Sprintf("%v/%v", prefix, filename)
	cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &objectKey,
		Body:        processedFile,
		ContentType: &filetype,
	})
	url := fmt.Sprintf("%v,%v",
		cfg.s3Bucket, objectKey)
	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video URL", err)
		return
	}

	responseVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		fmt.Println("Failed to sign video URL", err)
	}
	respondWithJSON(w, http.StatusOK, responseVideo)
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return video, fmt.Errorf("Invalid video resource location: %v", video.VideoURL)
	}
	url, err := generatePresignedURL(cfg.s3Client, parts[0], parts[1], 5*time.Minute)
	if err != nil {
		return database.Video{}, err
	}
	video.VideoURL = &url
	return video, nil
}

func generatePresignedURL(s3client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {

	presignClient := s3.NewPresignClient(s3client)
	object, err := presignClient.PresignGetObject(context.Background(),
		&s3.GetObjectInput{
			Bucket: &bucket,
			Key:    &key,
		},
		s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("Failed to sign URL: %v", err)
	}

	return object.URL, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	runner := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	buffer := bytes.Buffer{}
	errBuffer := bytes.Buffer{}
	runner.Stdout = &buffer
	runner.Stderr = &errBuffer
	err := runner.Run()
	if err != nil {
		return "", fmt.Errorf("%v: %v", err, errBuffer.String())
	}
	info := map[string]interface{}{}
	err = json.Unmarshal(buffer.Bytes(), &info)
	if err != nil {
		return "", err
	}

	width, ok := info["streams"].([]interface{})[0].(map[string]interface{})["width"].(float64)
	if !ok {
		return "", fmt.Errorf("Width not found")
	}
	height, ok := info["streams"].([]interface{})[0].(map[string]interface{})["height"].(float64)
	if !ok {
		return "", fmt.Errorf("Height not found")
	}

	aspectRatio := width / height
	result := "other"
	epsilon := 1e-3
	if math.Abs(16.0/9.0-aspectRatio) < epsilon {
		result = "landscape"
	}
	if math.Abs(9.0/16.0-aspectRatio) < epsilon {
		result = "portrait"
	}
	return result, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	processedPath := filePath + ".processed"

	runner := exec.Command(
		"ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		processedPath,
	)

	buffer := bytes.Buffer{}
	errBuffer := bytes.Buffer{}
	runner.Stdout = &buffer
	runner.Stderr = &errBuffer
	err := runner.Run()
	if err != nil {
		return "", fmt.Errorf("%v: %v", err, errBuffer.String())
	}
	return processedPath, nil
}
