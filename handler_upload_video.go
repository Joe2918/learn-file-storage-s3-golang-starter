package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	r.ParseMultipartForm(maxMemory)

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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
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
		respondWithError(w, http.StatusInternalServerError, "Could not write file to disk", err)
		return
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if _, err := tempFile.Seek(0, 0); err != nil {
		log.Fatal(err)
	}

	key := getAssetPath(mediaType)
	var newKey string
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error determining the aspect ratio", err)
		return
	}

	switch aspectRatio {
	case "16:9":
		newKey = filepath.Join("landscape", key)
	case "9:16":
		newKey = filepath.Join("portrait", key)
	default:
		newKey = filepath.Join("other", key)
	}

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		log.Fatal(err) // Handle potential opening errors
	}
	defer processedFile.Close()

	input := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(newKey),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(r.Context(), input)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	newURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, newKey)
	video.VideoURL = &newURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffprobe error: %v", err)
	}

	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %v", err)
	}

	if len(output.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	result := float64(output.Streams[0].Width) / float64(output.Streams[0].Height)
	aspect := fmt.Sprintf("%.2f", result)
	switch aspect {
	case "1.78":
		return "16:9", nil
	case "0.56":
		return "9:16", nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	processedFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg",
		"-i", filePath, "-c",
		"copy", "-movflags",
		"faststart", "-f",
		"mp4", processedFilePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return processedFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignedReq, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}
	return presignedReq.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) < 2 {
		return video, nil
	}

	presignedURL, err := generatePresignedURL(cfg.s3Client, parts[0], parts[1], 5*time.Minute)
	if err != nil {
		return video, err
	}

	video.VideoURL = &presignedURL
	return video, nil
}
