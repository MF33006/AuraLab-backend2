package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/AimMetal-jy/AuraLab-backend/BlueLM/config"
	"github.com/AimMetal-jy/AuraLab-backend/BlueLM/utils"
	"github.com/gin-gonic/gin"
)

// WhisperXHandler 代理对 Flask WhisperX 服务的请求
func WhisperXHandler(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 保存上传的文件
		uploadFilePath, err := utils.SaveUploadedFile(c, cfg.FilePaths.UploadDir)
		if err != nil {
			utils.AbortWithBadRequest(c, err, "Failed to save uploaded file")
			return
		}

		// 2. 异步调用 callWhisperXService 函数
		taskID, err := startWhisperXService(uploadFilePath, cfg)
		if err != nil {
			utils.AbortWithInternalServerError(c, err)
			return
		}

		// 3. 立即返回任务ID
		c.JSON(http.StatusOK, gin.H{"task_id": taskID})
	}
}

// EnhancedWhisperXHandler 增强版的WhisperX处理器，支持更多参数
func EnhancedWhisperXHandler(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 保存上传的文件
		uploadFilePath, err := utils.SaveUploadedFile(c, cfg.FilePaths.UploadDir)
		if err != nil {
			utils.AbortWithBadRequest(c, err, "Failed to save uploaded file")
			return
		}

		// 2. 获取额外的参数
		var params WhisperXParams
		params.Language = c.PostForm("language")
		params.ComputeType = c.PostForm("compute_type")
		params.EnableWordTimestamps = c.PostForm("enable_word_timestamps") == "true"
		params.EnableSpeakerDiarization = c.PostForm("enable_speaker_diarization") == "true"
		params.HuggingFaceToken = c.PostForm("huggingface_token")
		params.ModelName = c.PostForm("model_name")

		// 3. 异步调用增强的WhisperX服务
		taskID, err := startEnhancedWhisperXService(uploadFilePath, cfg, params)
		if err != nil {
			utils.AbortWithInternalServerError(c, err)
			return
		}

		// 4. 立即返回任务ID
		c.JSON(http.StatusOK, gin.H{"task_id": taskID})
	}
}

// WhisperXParams WhisperX处理参数结构
type WhisperXParams struct {
	Language                 string `json:"language,omitempty"`
	ComputeType              string `json:"compute_type,omitempty"`
	EnableWordTimestamps     bool   `json:"enable_word_timestamps"`
	EnableSpeakerDiarization bool   `json:"enable_speaker_diarization"`
	HuggingFaceToken         string `json:"huggingface_token,omitempty"`
	ModelName                string `json:"model_name,omitempty"`
}

// startWhisperXService 启动 WhisperX 服务并返回任务 ID
func startWhisperXService(filePath string, cfg *config.Config) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %v", err)
	}
	_, err = io.Copy(part, file)
	if err != nil {
		return "", fmt.Errorf("failed to copy file content: %v", err)
	}

	// 关闭 writer
	writer.Close()

	// 创建一个新的 HTTP 请求
	whisperxURL := fmt.Sprintf("%s/whisperx/process", cfg.WhisperX.URL)
	req, err := http.NewRequest("POST", whisperxURL, body)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	// 检查HTTP状态码
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("WhisperX service returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析 JSON 响应
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON response: %v, response body: %s", err, string(respBody))
	}

	// 检查是否有 task_id
	taskID, ok := result["task_id"].(string)
	if !ok {
		return "", fmt.Errorf("no task_id in response")
	}

	go pollWhisperXStatus(taskID, cfg)

	return taskID, nil
}

// pollWhisperXStatus 轮询 WhisperX 任务状态
func pollWhisperXStatus(taskID string, cfg *config.Config) {
	maxAttempts := 450 // 最大轮询次数 (450次 * 2秒 = 15分钟)
	attempts := 0

	for attempts < maxAttempts {
		attempts++
		time.Sleep(2 * time.Second) // 每2秒查询一次

		statusResp, err := http.Get(fmt.Sprintf("%s/whisperx/status/%s", cfg.WhisperX.URL, taskID))
		if err != nil {
			utils.Log.Errorf("failed to get task status: %v", err)
			continue
		}
		defer statusResp.Body.Close()

		statusBody, err := io.ReadAll(statusResp.Body)
		if err != nil {
			utils.Log.Errorf("failed to read status response body: %v", err)
			continue
		}

		var statusResult map[string]interface{}
		if err := json.Unmarshal(statusBody, &statusResult); err != nil {
			utils.Log.Errorf("failed to parse status JSON response: %v", err)
			continue
		}

		if status, ok := statusResult["status"].(string); ok && status == "completed" {
			// 将结果保存到文件
			fileName := fmt.Sprintf("%swhisperx_result_%s.json", cfg.FilePaths.DownloadDir, taskID)
			if err := os.WriteFile(fileName, statusBody, 0644); err != nil {
				utils.Log.Errorf("failed to save result to file: %v", err)
			}
			utils.Log.Infof("WhisperX task %s completed successfully", taskID)
			return
		} else if status == "failed" {
			utils.Log.Errorf("WhisperX task %s failed: %v", taskID, statusResult["error"])
			return
		}
	}

	// 如果达到最大轮询次数仍未完成，记录超时错误
	utils.Log.Errorf("WhisperX task %s polling timeout after %d attempts (15 minutes)", taskID, maxAttempts)
}

// startEnhancedWhisperXService 启动增强版WhisperX服务，支持更多参数
func startEnhancedWhisperXService(filePath string, cfg *config.Config, params WhisperXParams) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// 添加文件
	part, err := writer.CreateFormFile("file", filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %v", err)
	}
	_, err = io.Copy(part, file)
	if err != nil {
		return "", fmt.Errorf("failed to copy file content: %v", err)
	}

	// 添加其他参数
	if params.Language != "" {
		writer.WriteField("language", params.Language)
	}
	if params.ComputeType != "" {
		writer.WriteField("compute_type", params.ComputeType)
	}
	if params.EnableWordTimestamps {
		writer.WriteField("enable_word_timestamps", "true")
	}
	if params.EnableSpeakerDiarization {
		writer.WriteField("enable_speaker_diarization", "true")
	}
	if params.HuggingFaceToken != "" {
		writer.WriteField("huggingface_token", params.HuggingFaceToken)
	}
	if params.ModelName != "" {
		writer.WriteField("model_name", params.ModelName)
	}

	// 关闭 writer
	writer.Close()

	// 创建一个新的 HTTP 请求
	whisperxURL := fmt.Sprintf("%s/whisperx/process", cfg.WhisperX.URL)
	req, err := http.NewRequest("POST", whisperxURL, body)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	// 检查HTTP状态码
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("WhisperX service returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析 JSON 响应
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON response: %v, response body: %s", err, string(respBody))
	}

	// 检查是否有 task_id
	taskID, ok := result["task_id"].(string)
	if !ok {
		return "", fmt.Errorf("no task_id in response")
	}

	go pollWhisperXStatus(taskID, cfg)

	return taskID, nil
}
