package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

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

	// Get the video from the database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video", err)
		return
	}

	// Check if the user is the owner of the video
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized access", nil)
		return
	}

	videoFile, videoHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to get video file", err)
		return
	}
	defer videoFile.Close()

	// Get the media type of the video
	mediaType := videoHeader.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for video", nil)
		return
	}

	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type for video", err)
		return
	}

	if mediaType != "video/mp4" {
		fmt.Println("Unsupported file type:", mediaType)
		respondWithError(w, http.StatusBadRequest, "Unsupported file type for video, only mp4 is allowed", nil)
		return
	}

	// Create temp file to store the uploaded video
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Save the uploaded video to the temp file
	if _, err = io.Copy(tempFile, videoFile); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save video to temp file", err)
		return
	}

	//Reset tempFile file pointer so it can be read from the beginning
	if _, err = tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to reset temp file pointer", err)
		return
	}

	// Process the video for fast start
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open processed video file", err)
		return
	}
	defer processedFile.Close()

	// Get the aspect ratio of the video
	aspectRatio, err := getVideoAspectRatio(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video aspect ratio", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		aspectRatio = "landscape"
	case "9:16":
		aspectRatio = "portrait"
	default:
		aspectRatio = "other"
	}

	// Set the asset path
	key := getAssetPath(mediaType)
	key = aspectRatio + "/" + key

	// Put video object into AWS S3
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video to S3", err)
		return
	}

	// Set video URL to S3 URL
	videoURL := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, key)
	video.VideoURL = &videoURL

	// Update the video in the database
	if err = cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",     // Command to get video information
		"-v", "error", // Suppress unnecessary output
		"-print_format", "json", // Set output format to json
		"-show_streams", // Show streams information
		filePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	var result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return "", err
	}
	height := result.Streams[0].Height
	width := result.Streams[0].Width

	// Determine if the aspect ratio is 16:9, 9:16 or other
	if width == 0 || height == 0 {
		return "", errors.New("invalid video dimensions")
	}
	aspectRatio := float64(width) / float64(height)
	if aspectRatio > 1.7 && aspectRatio < 1.8 {
		return "16:9", nil
	} else if aspectRatio > 0.5 && aspectRatio < 0.6 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

// processVideoForFastStart processes the video to enable fast start by moving the moov atom to the beginning of the file
func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath, // Input file
		"-c", "copy", // Copy codec (no re-encoding)
		"-movflags", "faststart", // Move moov atom to the beginning of the file for fast start
		"-f", "mp4", // Set output format to mp4
		outputFilePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video for fast start: %s: %v", stderr.String(), err)
	}
	return outputFilePath, nil
}
