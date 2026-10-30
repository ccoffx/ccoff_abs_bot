package abs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

type AbsClient struct {
	BaseURL    string
	Token      string
	HttpClient *http.Client
}

// absClientUserAgent avoids the generic Go default User-Agent, which may be
// blocked by an upstream WAF while retaining a stable identifier for this bot.
const absClientUserAgent = "ccoff-abs-bot/1.0"

type AbsAPIError struct {
	Operation  string
	StatusCode int
	Message    string
}

func (e *AbsAPIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s，状态码: %d，%s", e.Operation, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s，状态码: %d", e.Operation, e.StatusCode)
}

func NewAbsClient() *AbsClient {
	c := &AbsClient{
		BaseURL:    config.AppConfig.AbsURL,
		Token:      config.AppConfig.AbsKey,
		HttpClient: &http.Client{Timeout: 15 * time.Second},
	}
	if config.AppConfig.IsTest() {
		transport, err := OpenMockTransport(filepath.Join(config.AppConfig.TestDataDir, "mock-abs.json"))
		if err != nil {
			panic(err)
		}
		c.HttpClient.Transport = transport
		// Hard-code the mock origin as a second barrier against configuration drift.
		c.BaseURL, c.Token = "https://abs.mock.invalid", "mock"
	}
	return c
}

func AbsUserPath(absUserID string) string {
	return "/api/users/" + url.PathEscape(absUserID)
}

func AbsUserListeningStatsPath(absUserID string) string {
	return AbsUserPath(absUserID) + "/listening-stats"
}

func AbsUserListeningSessionsPath(absUserID string, page int, itemsPerPage int) string {
	if page < 0 {
		page = 0
	}
	if itemsPerPage <= 0 {
		itemsPerPage = 100
	}
	return AbsUserPath(absUserID) + "/listening-sessions?itemsPerPage=" + strconv.Itoa(itemsPerPage) + "&page=" + strconv.Itoa(page)
}

func (c *AbsClient) SendRequest(method, path string, body []byte) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if c == nil {
		return nil, 0, fmt.Errorf("ABS 客户端未初始化")
	}
	if c.BaseURL == "" {
		return nil, 0, fmt.Errorf("ABS_API_URL 未配置")
	}
	if c.Token == "" {
		return nil, 0, fmt.Errorf("ABS_API_KEY 未配置")
	}
	httpClient := c.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewBuffer(body))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", absClientUserAgent)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	const maxAbsResponseBytes = 2 * 1024 * 1024
	limitedReader := io.LimitReader(resp.Body, maxAbsResponseBytes+1)

	respBody, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取 ABS 响应失败: %w", err)
	}

	if len(respBody) > maxAbsResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("ABS 响应体过大，已拒绝处理")
	}

	return respBody, resp.StatusCode, nil
}

func extractAbsUserIDFromResponse(resBody []byte) (string, error) {
	var resData map[string]interface{}
	if err := json.Unmarshal(resBody, &resData); err != nil {
		return "", fmt.Errorf("ABS 返回了非 JSON 数据: %w", err)
	}

	userObj, ok := resData["user"].(map[string]interface{})
	if !ok || userObj == nil {
		return "", fmt.Errorf("ABS 返回中缺少 user 对象")
	}

	id, ok := userObj["id"].(string)
	if !ok || id == "" {
		return "", fmt.Errorf("ABS 返回中缺少用户 ID")
	}

	return id, nil
}

func AbsResponseSnippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	s := textfmt.FormatDiagnosticTextForDisplay(string(body))
	if s == "" {
		return ""
	}
	return textfmt.TruncateRunes(s, 160)
}

func (c *AbsClient) RegisterUser(username, password string) (string, error) {
	if username == "" || password == "" {
		return "", fmt.Errorf("用户名或密码为空")
	}
	if config.AppConfig.AbsTemplateID == "" {
		return "", fmt.Errorf("ABS_TEMPLATE_USER_ID 未配置")
	}

	tplBody, code, err := c.SendRequest("GET", AbsUserPath(config.AppConfig.AbsTemplateID), nil)
	if err != nil {
		return "", fmt.Errorf("拉取权限模板失败，网络错误: %w", err)
	}
	if code != 200 {
		return "", &AbsAPIError{Operation: "拉取权限模板失败", StatusCode: code, Message: "响应: " + AbsResponseSnippet(tplBody)}
	}

	var tpl map[string]interface{}
	if err := json.Unmarshal(tplBody, &tpl); err != nil {
		return "", fmt.Errorf("解析权限模板失败: %w", err)
	}

	permissions, hasPermissions := tpl["permissions"]
	librariesAccessible, hasLibrariesAccessible := tpl["librariesAccessible"]

	if !hasPermissions {
		return "", fmt.Errorf("权限模板缺少 permissions 字段")
	}
	if !hasLibrariesAccessible {
		return "", fmt.Errorf("权限模板缺少 librariesAccessible 字段")
	}

	payload := map[string]interface{}{
		"username":            username,
		"password":            password,
		"type":                "user",
		"isActive":            true,
		"permissions":         permissions,
		"librariesAccessible": librariesAccessible,
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("生成开户请求失败: %w", err)
	}

	resBody, resCode, err := c.SendRequest("POST", "/api/users", reqBody)
	if err != nil {
		return "", fmt.Errorf("服务器拒绝创建，网络错误: %w", err)
	}

	if resCode != 200 && resCode != 201 {
		return "", &AbsAPIError{Operation: "服务器拒绝创建", StatusCode: resCode, Message: "响应: " + AbsResponseSnippet(resBody)}
	}

	userID, err := extractAbsUserIDFromResponse(resBody)
	if err != nil {
		return "", fmt.Errorf("开户成功但解析用户 ID 失败: %w", err)
	}

	return userID, nil
}

func (c *AbsClient) VerifyUser(username, password string) (string, error) {
	if username == "" || password == "" {
		return "", fmt.Errorf("用户名或密码为空")
	}

	payload := map[string]interface{}{
		"username": username,
		"password": password,
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("生成登录请求失败: %w", err)
	}

	resBody, code, err := c.SendRequest("POST", "/login", reqBody)
	if err != nil {
		return "", fmt.Errorf("账号不存在、密码错误或 ABS 网络异常")
	}
	if code != 200 {
		return "", fmt.Errorf("账号不存在或密码错误，状态码: %d", code)
	}

	userID, err := extractAbsUserIDFromResponse(resBody)
	if err != nil {
		return "", fmt.Errorf("登录成功但解析用户数据失败: %w", err)
	}

	return userID, nil
}

// 🔐 核心重构：绝对防崩的修改密码模块 (白名单提纯法)
func (c *AbsClient) UpdateAbsPassword(absUserID, newPassword string) error {
	body, code, err := c.SendRequest("GET", AbsUserPath(absUserID), nil)
	if err != nil {
		return fmt.Errorf("获取服务端数据失败，网络错误: %w", err)
	}
	if code != 200 {
		return &AbsAPIError{Operation: "获取服务端数据失败", StatusCode: code}
	}

	var resData map[string]interface{}
	if err := json.Unmarshal(body, &resData); err != nil {
		return fmt.Errorf("底层解析异常，服务器返回了非标准数据")
	}

	if resData == nil {
		return fmt.Errorf("拉取到的用户原生数据为空")
	}

	if id, ok := resData["id"].(string); !ok || id != absUserID {
		return fmt.Errorf("ABS 用户数据 ID 不匹配")
	}

	userObj := resData

	if userObj == nil {
		return fmt.Errorf("用户对象剥离失败，无法注入新密码")
	}

	// 🚨 终极修复：放弃“打地鼠”式的删除，采用“白名单提纯法”
	// 只保留服务端允许修改的核心字段，彻底抛弃 token、id、时间戳等所有只读属性
	cleanPayload := map[string]interface{}{
		"password":            newPassword,
		"username":            userObj["username"],
		"type":                userObj["type"],
		"isActive":            userObj["isActive"],
		"permissions":         userObj["permissions"],
		"librariesAccessible": userObj["librariesAccessible"],
	}

	reqBody, err := json.Marshal(cleanPayload)
	if err != nil {
		return fmt.Errorf("生成 ABS 密码更新请求失败: %w", err)
	}
	resBody, patchCode, err := c.SendRequest("PATCH", AbsUserPath(absUserID), reqBody)

	if err != nil {
		return fmt.Errorf("ABS底层拒绝覆盖，网络错误: %w", err)
	}
	if patchCode < 200 || patchCode >= 300 {
		return &AbsAPIError{Operation: "ABS底层拒绝覆盖", StatusCode: patchCode, Message: "响应: " + AbsResponseSnippet(resBody)}
	}

	return nil
}

// 🔐 修改用户名模块 (沿用密码模块的白名单提纯法)
func (c *AbsClient) UpdateAbsUsername(absUserID, newUsername string) error {
	newUsername = strings.TrimSpace(newUsername)
	if absUserID == "" || newUsername == "" {
		return fmt.Errorf("ABS 用户ID或新用户名为空")
	}

	body, code, err := c.SendRequest("GET", AbsUserPath(absUserID), nil)
	if err != nil {
		return fmt.Errorf("获取服务端数据失败，网络错误: %w", err)
	}
	if code != 200 {
		return &AbsAPIError{Operation: "获取服务端数据失败", StatusCode: code}
	}

	var resData map[string]interface{}
	if err := json.Unmarshal(body, &resData); err != nil {
		return fmt.Errorf("底层解析异常，服务器返回了非标准数据")
	}
	if resData == nil {
		return fmt.Errorf("拉取到的用户原生数据为空")
	}

	if id, ok := resData["id"].(string); !ok || id != absUserID {
		return fmt.Errorf("ABS 用户数据 ID 不匹配")
	}

	userObj := resData
	if userObj == nil {
		return fmt.Errorf("用户对象剥离失败，无法注入新用户名")
	}

	// 白名单提纯：只携带服务端允许写回的核心字段，丢弃 token/id/时间戳等只读属性。
	// 注意：不携带 password 字段，避免覆盖用户现有密码。
	cleanPayload := map[string]interface{}{
		"username":            newUsername,
		"type":                userObj["type"],
		"isActive":            userObj["isActive"],
		"permissions":         userObj["permissions"],
		"librariesAccessible": userObj["librariesAccessible"],
	}

	reqBody, err := json.Marshal(cleanPayload)
	if err != nil {
		return fmt.Errorf("生成 ABS 用户名更新请求失败: %w", err)
	}
	resBody, patchCode, err := c.SendRequest("PATCH", AbsUserPath(absUserID), reqBody)
	if err != nil {
		return fmt.Errorf("ABS底层拒绝覆盖，网络错误: %w", err)
	}
	if patchCode < 200 || patchCode >= 300 {
		return &AbsAPIError{Operation: "ABS底层拒绝覆盖", StatusCode: patchCode, Message: "响应: " + AbsResponseSnippet(resBody)}
	}

	return nil
}

func (c *AbsClient) DeleteUser(absUserID string) error {
	if absUserID == "" {
		return nil
	}

	_, code, err := c.SendRequest("DELETE", AbsUserPath(absUserID), nil)
	if err != nil {
		return fmt.Errorf("删除失败，ABS 网络错误: %w", err)
	}

	if code < 200 || code >= 300 {
		return &AbsAPIError{Operation: "删除失败", StatusCode: code}
	}

	return nil
}

func IsAbsNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	var apiErr *AbsAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}

	return false
}

func PositiveListeningSeconds(seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return seconds
}

func sumPositiveABSListeningDays(days map[string]float64) float64 {
	var total float64
	for _, seconds := range days {
		total += PositiveListeningSeconds(seconds)
	}
	return total
}

func AuthoritativeABSListeningTotalSeconds(totalTime float64, days map[string]float64) float64 {
	if totalTime > 0 {
		return totalTime
	}
	return sumPositiveABSListeningDays(days)
}

func (c *AbsClient) GetServerStats() string {
	startTime := time.Now()

	// 1. 获取注册用户总数 (GET /api/users)
	uBody, uCode, uErr := c.SendRequest("GET", "/api/users", nil)

	// 2. 获取所有活动会话 (GET /api/sessions)
	sBody, sCode, sErr := c.SendRequest("GET", "/api/sessions", nil)

	latency := time.Since(startTime)

	if uErr != nil {
		log.Printf("⚠️ 服务器监控 ABS 用户列表读取失败: err=%s", textfmt.FormatPlainError(uErr))
	}
	if sErr != nil {
		log.Printf("⚠️ 服务器监控 ABS 会话列表读取失败: err=%s", textfmt.FormatPlainError(sErr))
	}
	if uCode != 200 || sCode != 200 {
		return fmt.Sprintf("📈 **服务器实时监控**\n\n❌ **服务连接失败**\n🔴 服务器状态: **服务宕机 (Offline)**\n📊 错误状态码: Users(%d), Sessions(%d)", uCode, sCode)
	}

	userCountText := "读取失败"
	activeSessionCountText := "读取失败"
	var pointsUserCount int64
	pointsUserCountText := "0"
	if err := storage.DB.Model(&model.User{}).Count(&pointsUserCount).Error; err != nil {
		log.Printf("⚠️ 统计积分用户注册总数失败: %s", textfmt.FormatPlainError(err))
		pointsUserCountText = "读取失败"
	} else {
		pointsUserCountText = strconv.FormatInt(pointsUserCount, 10)
	}

	// 解析用户总数
	var uRes map[string]interface{}
	if err := json.Unmarshal(uBody, &uRes); err == nil {
		if uList, ok := uRes["users"].([]interface{}); ok {
			userCountText = strconv.Itoa(len(uList))
		} else {
			log.Printf("⚠️ 服务器监控 ABS 用户列表结构异常: err=%s", textfmt.FormatPlainError(fmt.Errorf("users field missing or invalid")))
		}
	} else {
		log.Printf("⚠️ 服务器监控 ABS 用户列表解析失败: %s", textfmt.FormatPlainError(err))
	}

	// 3. 完美对齐 ABS 控制面板的会话解析逻辑
	var sessionsList []interface{}
	var sRes map[string]interface{}

	if err := json.Unmarshal(sBody, &sRes); err == nil {
		if list, ok := sRes["sessions"].([]interface{}); ok {
			sessionsList = list
		} else {
			log.Printf("⚠️ 服务器监控 ABS 会话列表结构异常: err=%s", textfmt.FormatPlainError(fmt.Errorf("sessions field missing or invalid")))
		}
	} else {
		log.Printf("⚠️ 服务器监控 ABS 会话列表解析失败: %s", textfmt.FormatPlainError(err))
	}

	// 🚨 修正重点：直接统计 ABS 服务端当前挂载的真实活动会话总数，不再进行有无点开书籍的严格过滤
	if sessionsList != nil {
		activeSessionCountText = strconv.Itoa(len(sessionsList))
	}

	// 4. 智能动态评估服务器响应延迟
	statusText := "🟢 运行良好 (Excellent)"
	avgLatencyMs := latency.Milliseconds() / 2

	switch {
	case avgLatencyMs > 800:
		statusText = fmt.Sprintf("🟠 服务器拥堵 (Congested) [%dms]", avgLatencyMs)
	case avgLatencyMs > 200:
		statusText = fmt.Sprintf("🟡 负载轻微 (Heavy) [%dms]", avgLatencyMs)
	default:
		statusText = fmt.Sprintf("🟢 运行良好 (Healthy) [%dms]", avgLatencyMs)
	}

	dailyRefreshAt, dailyRefreshAtAvailable := storage.FormatSystemConfigTimeForStatus(storage.DailyListeningRefreshLastAtKey)
	if dailyRefreshAtAvailable && dailyRefreshAt == "无" {
		dailyRefreshAt = "尚未执行"
	}
	dailyRefreshSuccess, _ := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastSuccessKey, "0")
	if dailyRefreshSuccess == "" {
		dailyRefreshSuccess = "0"
	}
	dailyRefreshTotal, _ := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastTotalKey, "0")
	if dailyRefreshTotal == "" {
		dailyRefreshTotal = "0"
	}
	dailyRefreshSkipped, _ := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastSkippedKey, "0")
	if dailyRefreshSkipped == "" {
		dailyRefreshSkipped = "0"
	}
	dailyRefreshError, _ := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastErrorKey, "无")

	// 5. 精美渲染输出
	return fmt.Sprintf("📈 **服务器实时监控**\n\n👥 ABS 用户注册总数: `%s` 人\n🧮 积分用户注册总数: `%s` 人\n🎧 活跃听书会话: `%s` 个\n⚡️ 平均响应延迟: `%dms`\n🛡️ 服务器状态: %s\n\n🌅 **每日净修为刷新**\n上次刷新：`%s`\n成功/总数：`%s/%s`\n跳过：`%s`\n最近错误：`%s`",
		textfmt.EscapeMarkdown(userCountText), textfmt.EscapeMarkdown(pointsUserCountText), textfmt.EscapeMarkdown(activeSessionCountText), avgLatencyMs, statusText,
		dailyRefreshAt, dailyRefreshSuccess, dailyRefreshTotal, dailyRefreshSkipped, textfmt.FormatSystemConfigErrorForMarkdown(dailyRefreshError))
}

// 🛑 核心功能：开关账号权限
func (c *AbsClient) SetUserActiveStatus(absUserID string, isActive bool) error {
	if absUserID == "" {
		return fmt.Errorf("ABS 用户 ID 为空，无法切换状态")
	}

	body, code, err := c.SendRequest("GET", AbsUserPath(absUserID), nil)
	if err != nil {
		return fmt.Errorf("拉取 ABS 用户失败，网络错误: %w", err)
	}
	if code != 200 {
		return &AbsAPIError{Operation: "拉取 ABS 用户失败", StatusCode: code}
	}

	var resData map[string]interface{}
	if err := json.Unmarshal(body, &resData); err != nil {
		return fmt.Errorf("解析 ABS 用户数据失败: %w", err)
	}

	if id, ok := resData["id"].(string); !ok || id != absUserID {
		return fmt.Errorf("ABS 用户数据 ID 不匹配")
	}

	userObj := resData
	if userObj == nil {
		return fmt.Errorf("获取不到 ABS 用户数据实体")
	}

	cleanPayload := map[string]interface{}{
		"isActive":            isActive,
		"username":            userObj["username"],
		"type":                userObj["type"],
		"permissions":         userObj["permissions"],
		"librariesAccessible": userObj["librariesAccessible"],
	}

	reqBody, err := json.Marshal(cleanPayload)
	if err != nil {
		return fmt.Errorf("生成 ABS 状态切换请求失败: %w", err)
	}

	resBody, patchCode, err := c.SendRequest("PATCH", AbsUserPath(absUserID), reqBody)
	if err != nil {
		return fmt.Errorf("状态切换失败，ABS 网络错误: %w", err)
	}
	if patchCode < 200 || patchCode >= 300 {
		return &AbsAPIError{Operation: "状态切换失败", StatusCode: patchCode, Message: "响应: " + AbsResponseSnippet(resBody)}
	}

	return nil
}

func (c *AbsClient) GetUserActiveStatus(absUserID string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("ABS 客户端未初始化")
	}
	if absUserID == "" {
		return false, fmt.Errorf("ABS 用户 ID 为空，无法读取状态")
	}

	body, code, err := c.SendRequest("GET", AbsUserPath(absUserID), nil)
	if err != nil {
		return false, fmt.Errorf("拉取 ABS 用户失败，网络错误: %w", err)
	}
	if code != 200 {
		return false, &AbsAPIError{Operation: "拉取 ABS 用户失败", StatusCode: code}
	}

	var resData map[string]interface{}
	if err := json.Unmarshal(body, &resData); err != nil {
		return false, fmt.Errorf("解析 ABS 用户数据失败: %w", err)
	}

	if id, ok := resData["id"].(string); !ok || id != absUserID {
		return false, fmt.Errorf("ABS 用户数据 ID 不匹配")
	}

	userObj := resData
	if userObj == nil {
		return false, fmt.Errorf("获取不到 ABS 用户数据实体")
	}

	activeRaw, ok := userObj["isActive"]
	if !ok {
		return false, fmt.Errorf("ABS 用户数据缺少 isActive 字段")
	}

	active, ok := activeRaw.(bool)
	if !ok {
		return false, fmt.Errorf("ABS 用户 isActive 字段异常")
	}
	return active, nil
}

// 定义用于解析 ABS 用户列表的极简结构体
type AbsUserMinified struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Type     string `json:"type"` // 用于识别 root 和 admin
}

// 获取 ABS 服务端的所有用户列表
func (c *AbsClient) GetAllUsers() ([]AbsUserMinified, error) {
	body, code, err := c.SendRequest("GET", "/api/users", nil)

	if err != nil {
		return nil, fmt.Errorf("网络错误")
	}
	if code != 200 {
		return nil, &AbsAPIError{Operation: "拉取 ABS 用户列表失败", StatusCode: code}
	}

	var response struct {
		Users []AbsUserMinified `json:"users"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析 ABS 用户列表失败: %w", err)
	}
	if response.Users == nil {
		return nil, fmt.Errorf("ABS 用户列表缺少 users 数组")
	}
	return response.Users, nil
}

var DefaultClient *AbsClient
