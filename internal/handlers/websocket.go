package handlers

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fire-tv-rooms-ecs/internal/config"
	"fire-tv-rooms-ecs/internal/models"
	"fire-tv-rooms-ecs/internal/services"
	"fire-tv-rooms-ecs/internal/utils"

	"github.com/gobwas/ws"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

// WebSocketHandler handles WebSocket connections and HTTP endpoints
type WebSocketHandler struct {
	connectionManager *services.ConnectionManager
	redisService      *services.RedisService
	roomService       *services.RoomService
	config            *config.Config

	// Request validation
	maxMessageSize int64
	allowedOrigins map[string]bool
	rateLimiter    *RequestRateLimiter
}

// RequestRateLimiter implements per-IP rate limiting
type RequestRateLimiter struct {
	requests map[string]*IPRateLimit
	cleanup  chan string
}

// IPRateLimit tracks rate limiting for a specific IP
type IPRateLimit struct {
	count     int
	resetTime time.Time
	blocked   bool
}

// ConnectionRequest represents a WebSocket connection request
type ConnectionRequest struct {
	UserID   string `json:"user_id" validate:"required,min=1,max=50"`
	RoomID   string `json:"room_id" validate:"required,min=1,max=50"`
	DeviceID string `json:"device_id" validate:"required,min=1,max=100"`
	Platform string `json:"platform" validate:"required,oneof=firetv mobile web"`
	Token    string `json:"token" validate:"required,min=10"`
}

// RoomCreateRequest represents a room creation request
type RoomCreateRequest struct {
	Name      string                 `json:"name" validate:"required,min=1,max=100"`
	MaxUsers  int                    `json:"max_users" validate:"min=2,max=50"`
	CreatedBy string                 `json:"created_by" validate:"required,min=1,max=50"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// RoomResponse represents a room response
type RoomResponse struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	CreatedBy string                 `json:"created_by"`
	CreatedAt time.Time              `json:"created_at"`
	MaxUsers  int                    `json:"max_users"`
	UserCount int                    `json:"user_count"`
	IsActive  bool                   `json:"is_active"`
	Metadata  map[string]interface{} `json:"metadata"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// HealthResponse represents a health check response
type HealthResponse struct {
	Status      string            `json:"status"`
	Timestamp   time.Time         `json:"timestamp"`
	Version     string            `json:"version"`
	Connections int64             `json:"connections"`
	Uptime      string            `json:"uptime"`
	Services    map[string]string `json:"services"`
}

// NewWebSocketHandler creates a new WebSocket handler
func NewWebSocketHandler(
	cm *services.ConnectionManager,
	rs *services.RedisService,
	roomSvc *services.RoomService,
	cfg *config.Config,
) (*WebSocketHandler, error) {

	if cm == nil {
		return nil, fmt.Errorf("connection manager cannot be nil")
	}
	if rs == nil {
		return nil, fmt.Errorf("redis service cannot be nil")
	}
	if roomSvc == nil {
		return nil, fmt.Errorf("room service cannot be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	// Initialize allowed origins
	allowedOrigins := make(map[string]bool)
	allowedOrigins["https://firetv.amazon.com"] = true
	allowedOrigins["https://mobile.firetv.com"] = true
	allowedOrigins["https://web.firetv.com"] = true
	if cfg.App.Environment == "development" || cfg.App.Environment == "staging" {
		allowedOrigins["http://localhost:3000"] = true
		allowedOrigins["http://localhost:8080"] = true
		allowedOrigins["http://localhost:5000"] = true
	}

	handler := &WebSocketHandler{
		connectionManager: cm,
		redisService:      rs,
		roomService:       roomSvc,
		config:            cfg,
		maxMessageSize:    1024 * 1024, // 1MB max message size
		allowedOrigins:    allowedOrigins,
		rateLimiter:       NewRequestRateLimiter(),
	}

	utils.Info("WebSocket handler initialized",
		zap.Int("allowed_origins", len(allowedOrigins)),
		zap.Int64("max_message_size", handler.maxMessageSize))

	return handler, nil
}

// NewRequestRateLimiter creates a new request rate limiter
func NewRequestRateLimiter() *RequestRateLimiter {
	rl := &RequestRateLimiter{
		requests: make(map[string]*IPRateLimit),
		cleanup:  make(chan string, 1000),
	}

	// Start cleanup goroutine
	go rl.cleanupExpiredEntries()

	return rl
}

// IsAllowed checks if a request from the given IP is allowed
func (rl *RequestRateLimiter) IsAllowed(ip string) bool {
	if ip == "" {
		return false
	}

	now := time.Now()

	// Get or create rate limit entry
	entry, exists := rl.requests[ip]
	if !exists {
		rl.requests[ip] = &IPRateLimit{
			count:     1,
			resetTime: now.Add(time.Minute),
			blocked:   false,
		}
		return true
	}

	// Reset if time window expired
	if now.After(entry.resetTime) {
		entry.count = 1
		entry.resetTime = now.Add(time.Minute)
		entry.blocked = false
		return true
	}

	// Check if blocked
	if entry.blocked {
		return false
	}

	// Increment count
	entry.count++

	// Block if exceeded limit (100 requests per minute)
	if entry.count > 100 {
		entry.blocked = true
		utils.Warn("IP rate limited",
			zap.String("ip", ip),
			zap.Int("count", entry.count))
		return false
	}

	return true
}

// cleanupExpiredEntries removes expired rate limit entries
func (rl *RequestRateLimiter) cleanupExpiredEntries() {
	ticker := time.NewTicker(time.Minute * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			for ip, entry := range rl.requests {
				if now.After(entry.resetTime.Add(time.Minute * 5)) {
					delete(rl.requests, ip)
				}
			}
		case ip := <-rl.cleanup:
			delete(rl.requests, ip)
		}
	}
}

// RegisterRoutes registers HTTP routes
func (h *WebSocketHandler) RegisterRoutes(router *mux.Router) {
	// WebSocket endpoint
	router.HandleFunc("/ws", h.handleWebSocketConnection).Methods("GET")

	// REST API endpoints
	api := router.PathPrefix("/api/v1").Subrouter()
	api.Use(h.corsMiddleware)
	api.Use(h.rateLimitMiddleware)
	api.Use(h.loggingMiddleware)

	// Room management
	api.HandleFunc("/rooms", h.createRoom).Methods("POST")
	api.HandleFunc("/rooms/{roomId}", h.getRoom).Methods("GET")
	api.HandleFunc("/rooms/{roomId}", h.deleteRoom).Methods("DELETE")
	api.HandleFunc("/rooms", h.listRooms).Methods("GET")

	// Health check
	router.HandleFunc("/health", h.healthCheck).Methods("GET")
	router.HandleFunc("/ready", h.readinessCheck).Methods("GET")

	utils.Info("WebSocket handler routes registered")
}

// handleWebSocketConnection handles WebSocket upgrade requests
func (h *WebSocketHandler) handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Get client IP
	clientIP := h.getClientIP(r)

	// Rate limiting
	if !h.rateLimiter.IsAllowed(clientIP) {
		utils.Warn("WebSocket connection rate limited",
			zap.String("ip", clientIP))
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Validate origin
	if !h.validateOrigin(r) {
		utils.Warn("WebSocket connection rejected - invalid origin",
			zap.String("origin", r.Header.Get("Origin")),
			zap.String("ip", clientIP))
		http.Error(w, "Invalid origin", http.StatusForbidden)
		return
	}

	// Parse and validate connection parameters
	connReq, err := h.parseConnectionRequest(r)
	if err != nil {
		utils.Error("Invalid connection request",
			zap.Error(err),
			zap.String("ip", clientIP))
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Validate authentication token
	if err := h.validateAuthToken(connReq.Token, connReq.UserID); err != nil {
		utils.Error("Authentication failed",
			zap.Error(err),
			zap.String("user_id", connReq.UserID),
			zap.String("ip", clientIP))
		http.Error(w, "Authentication failed", http.StatusUnauthorized)
		return
	}

	// Validate room exists and user can join
	if err := h.validateRoomAccess(connReq.RoomID, connReq.UserID); err != nil {
		utils.Error("Room access denied",
			zap.Error(err),
			zap.String("room_id", connReq.RoomID),
			zap.String("user_id", connReq.UserID),
			zap.String("ip", clientIP))
		http.Error(w, fmt.Sprintf("Room access denied: %v", err), http.StatusForbidden)
		return
	}

	// Upgrade to WebSocket
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		utils.Error("WebSocket upgrade failed",
			zap.Error(err),
			zap.String("ip", clientIP))
		return
	}

	// Add connection to manager
	wsConn, err := h.connectionManager.AddConnection(
		conn,
		connReq.UserID,
		connReq.RoomID,
		connReq.DeviceID,
		connReq.Platform,
	)
	if err != nil {
		utils.Error("Failed to add connection",
			zap.Error(err),
			zap.String("user_id", connReq.UserID),
			zap.String("room_id", connReq.RoomID),
			zap.String("ip", clientIP))
		conn.Close()
		return
	}

	utils.Info("WebSocket connection established",
		zap.String("connection_id", wsConn.ID),
		zap.String("user_id", connReq.UserID),
		zap.String("room_id", connReq.RoomID),
		zap.String("platform", connReq.Platform),
		zap.String("ip", clientIP),
		zap.Duration("setup_time", time.Since(startTime)))
}

// parseConnectionRequest parses and validates connection request parameters
func (h *WebSocketHandler) parseConnectionRequest(r *http.Request) (*ConnectionRequest, error) {
	query := r.URL.Query()

	connReq := &ConnectionRequest{
		UserID:   strings.TrimSpace(query.Get("user_id")),
		RoomID:   strings.TrimSpace(query.Get("room_id")),
		DeviceID: strings.TrimSpace(query.Get("device_id")),
		Platform: strings.TrimSpace(query.Get("platform")),
		Token:    strings.TrimSpace(query.Get("token")),
	}

	// Validate required fields
	if connReq.UserID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	if connReq.RoomID == "" {
		return nil, fmt.Errorf("room_id is required")
	}
	if connReq.DeviceID == "" {
		return nil, fmt.Errorf("device_id is required")
	}
	if connReq.Platform == "" {
		return nil, fmt.Errorf("platform is required")
	}
	if connReq.Token == "" {
		return nil, fmt.Errorf("token is required")
	}

	// Validate field lengths
	if len(connReq.UserID) > 50 {
		return nil, fmt.Errorf("user_id too long (max 50 characters)")
	}
	if len(connReq.RoomID) > 50 {
		return nil, fmt.Errorf("room_id too long (max 50 characters)")
	}
	if len(connReq.DeviceID) > 100 {
		return nil, fmt.Errorf("device_id too long (max 100 characters)")
	}

	// Validate platform
	validPlatforms := map[string]bool{
		"firetv": true,
		"mobile": true,
		"web":    true,
	}
	if !validPlatforms[connReq.Platform] {
		return nil, fmt.Errorf("invalid platform: %s", connReq.Platform)
	}

	// Validate token format
	if len(connReq.Token) < 10 {
		return nil, fmt.Errorf("token too short (min 10 characters)")
	}

	return connReq, nil
}

// validateOrigin validates the request origin
func (h *WebSocketHandler) validateOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Allow requests without origin (e.g., from native apps)
		return true
	}

	return h.allowedOrigins[origin]
}

// validateAuthToken validates the authentication token
func (h *WebSocketHandler) validateAuthToken(token, userID string) error {
	if token == "" {
		return fmt.Errorf("token cannot be empty")
	}
	if userID == "" {
		return fmt.Errorf("user ID cannot be empty")
	}

	// TODO: Implement proper JWT token validation
	// For now, just check token format and length
	if len(token) < 10 {
		return fmt.Errorf("invalid token format")
	}

	// In production, validate JWT token here
	// - Verify signature
	// - Check expiration
	// - Validate user ID claim
	// - Check token blacklist

	return nil
}

// validateRoomAccess validates that a user can access a room
func (h *WebSocketHandler) validateRoomAccess(roomID, userID string) error {
	if roomID == "" {
		return fmt.Errorf("room ID cannot be empty")
	}
	if userID == "" {
		return fmt.Errorf("user ID cannot be empty")
	}

	// Get room information
	room, err := h.roomService.GetRoom(roomID)
	if err != nil {
		return fmt.Errorf("room not found: %w", err)
	}

	// Check if room is active
	if !room.IsActive {
		return fmt.Errorf("room is not active")
	}

	// Check room capacity
	currentUsers := h.connectionManager.GetRoomConnectionCount(roomID)
	if currentUsers >= room.MaxUsers {
		return fmt.Errorf("room is full (%d/%d)", currentUsers, room.MaxUsers)
	}

	// TODO: Add additional access controls
	// - Check if user is banned from room
	// - Check if room requires invitation
	// - Check user permissions

	return nil
}

// createRoom handles room creation requests
func (h *WebSocketHandler) createRoom(w http.ResponseWriter, r *http.Request) {
	var req RoomCreateRequest
	if err := h.parseJSONRequest(r, &req); err != nil {
		h.sendErrorResponse(w, http.StatusBadRequest, "Invalid request", err.Error())
		return
	}

	// Validate request
	if err := h.validateRoomCreateRequest(&req); err != nil {
		h.sendErrorResponse(w, http.StatusBadRequest, "Validation failed", err.Error())
		return
	}

	// Create room
	room := models.NewRoom(req.Name, req.CreatedBy, req.MaxUsers)
	if req.Metadata != nil {
		room.Metadata = req.Metadata
	}

	if err := h.roomService.CreateRoom(room); err != nil {
		utils.Error("Failed to create room",
			zap.Error(err),
			zap.String("name", req.Name),
			zap.String("created_by", req.CreatedBy))
		h.sendErrorResponse(w, http.StatusInternalServerError, "Failed to create room", "")
		return
	}

	// Send response
	response := &RoomResponse{
		ID:        room.ID,
		Name:      room.Name,
		CreatedBy: room.CreatedBy,
		CreatedAt: room.CreatedAt,
		MaxUsers:  room.MaxUsers,
		UserCount: 0,
		IsActive:  room.IsActive,
		Metadata:  room.Metadata,
	}

	h.sendJSONResponse(w, http.StatusCreated, response)

	utils.Info("Room created successfully",
		zap.String("room_id", room.ID),
		zap.String("name", room.Name),
		zap.String("created_by", room.CreatedBy))
}

// getRoom handles room retrieval requests
func (h *WebSocketHandler) getRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomId"]

	if roomID == "" {
		h.sendErrorResponse(w, http.StatusBadRequest, "Room ID is required", "")
		return
	}

	room, err := h.roomService.GetRoom(roomID)
	if err != nil {
		utils.Error("Failed to get room",
			zap.Error(err),
			zap.String("room_id", roomID))
		h.sendErrorResponse(w, http.StatusNotFound, "Room not found", "")
		return
	}

	userCount := h.connectionManager.GetRoomConnectionCount(roomID)

	response := &RoomResponse{
		ID:        room.ID,
		Name:      room.Name,
		CreatedBy: room.CreatedBy,
		CreatedAt: room.CreatedAt,
		MaxUsers:  room.MaxUsers,
		UserCount: userCount,
		IsActive:  room.IsActive,
		Metadata:  room.Metadata,
	}

	h.sendJSONResponse(w, http.StatusOK, response)
}

// deleteRoom handles room deletion requests
func (h *WebSocketHandler) deleteRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomId"]

	if roomID == "" {
		h.sendErrorResponse(w, http.StatusBadRequest, "Room ID is required", "")
		return
	}

	// TODO: Validate user has permission to delete room

	if err := h.roomService.DeleteRoom(roomID); err != nil {
		utils.Error("Failed to delete room",
			zap.Error(err),
			zap.String("room_id", roomID))
		h.sendErrorResponse(w, http.StatusInternalServerError, "Failed to delete room", "")
		return
	}

	w.WriteHeader(http.StatusNoContent)

	utils.Info("Room deleted successfully",
		zap.String("room_id", roomID))
}

// listRooms handles room listing requests
func (h *WebSocketHandler) listRooms(w http.ResponseWriter, r *http.Request) {
	// Parse query parameters
	query := r.URL.Query()
	limitStr := query.Get("limit")
	offsetStr := query.Get("offset")

	limit := 20 // default
	offset := 0 // default

	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}

	if offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	// Parse additional filter parameters
	var filter *services.RoomFilter
	if createdBy := query.Get("created_by"); createdBy != "" {
		if filter == nil {
			filter = &services.RoomFilter{}
		}
		filter.CreatedBy = createdBy
	}

	if isActiveStr := query.Get("is_active"); isActiveStr != "" {
		if isActive, err := strconv.ParseBool(isActiveStr); err == nil {
			if filter == nil {
				filter = &services.RoomFilter{}
			}
			filter.IsActive = &isActive
		}
	}

	if minUsersStr := query.Get("min_users"); minUsersStr != "" {
		if minUsers, err := strconv.Atoi(minUsersStr); err == nil && minUsers > 0 {
			if filter == nil {
				filter = &services.RoomFilter{}
			}
			filter.MinUsers = minUsers
		}
	}

	if maxUsersStr := query.Get("max_users"); maxUsersStr != "" {
		if maxUsers, err := strconv.Atoi(maxUsersStr); err == nil && maxUsers > 0 {
			if filter == nil {
				filter = &services.RoomFilter{}
			}
			filter.MaxUsers = maxUsers
		}
	}

	// Parse sorting parameters
	var sort *services.RoomSort
	if sortField := query.Get("sort"); sortField != "" {
		sort = &services.RoomSort{
			Field: sortField,
			Desc:  query.Get("order") == "desc",
		}
	}

	// Call the room service with all required parameters
	rooms, err := h.roomService.ListRooms(limit, offset, filter, sort)
	if err != nil {
		utils.Error("Failed to list rooms", zap.Error(err))
		h.sendErrorResponse(w, http.StatusInternalServerError, "Failed to list rooms", "")
		return
	}

	var responses []*RoomResponse
	for _, room := range rooms {
		userCount := h.connectionManager.GetRoomConnectionCount(room.ID)
		responses = append(responses, &RoomResponse{
			ID:        room.ID,
			Name:      room.Name,
			CreatedBy: room.CreatedBy,
			CreatedAt: room.CreatedAt,
			MaxUsers:  room.MaxUsers,
			UserCount: userCount,
			IsActive:  room.IsActive,
			Metadata:  room.Metadata,
		})
	}

	h.sendJSONResponse(w, http.StatusOK, map[string]interface{}{
		"rooms":  responses,
		"limit":  limit,
		"offset": offset,
		"total":  len(responses),
		"filter": filter,
		"sort":   sort,
	})
}

// healthCheck handles health check requests
func (h *WebSocketHandler) healthCheck(w http.ResponseWriter, r *http.Request) {
	services := make(map[string]string)

	// Check Redis
	if err := h.redisService.HealthCheck(); err != nil {
		services["redis"] = "unhealthy"
	} else {
		services["redis"] = "healthy"
	}

	// Check Connection Manager
	if err := h.connectionManager.HealthCheck(); err != nil {
		services["connection_manager"] = "unhealthy"
	} else {
		services["connection_manager"] = "healthy"
	}

	// Check Room Service
	if err := h.roomService.HealthCheck(); err != nil {
		services["room_service"] = "unhealthy"
	} else {
		services["room_service"] = "healthy"
	}

	status := "healthy"
	for _, serviceStatus := range services {
		if serviceStatus == "unhealthy" {
			status = "unhealthy"
			break
		}
	}

	response := &HealthResponse{
		Status:      status,
		Timestamp:   time.Now(),
		Version:     "1.0.0",
		Connections: h.connectionManager.GetConnectionCount(),
		Uptime:      time.Since(time.Now()).String(), // TODO: Track actual uptime
		Services:    services,
	}

	statusCode := http.StatusOK
	if status == "unhealthy" {
		statusCode = http.StatusServiceUnavailable
	}

	h.sendJSONResponse(w, statusCode, response)
}

// readinessCheck handles readiness check requests
func (h *WebSocketHandler) readinessCheck(w http.ResponseWriter, r *http.Request) {
	// Simple readiness check - just return OK if service is running
	h.sendJSONResponse(w, http.StatusOK, map[string]interface{}{
		"status":    "ready",
		"timestamp": time.Now(),
	})
}

// Middleware functions

// corsMiddleware handles CORS headers
func (h *WebSocketHandler) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if h.allowedOrigins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware handles rate limiting
func (h *WebSocketHandler) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := h.getClientIP(r)

		if !h.rateLimiter.IsAllowed(clientIP) {
			h.sendErrorResponse(w, http.StatusTooManyRequests, "Rate limit exceeded", "")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs HTTP requests
func (h *WebSocketHandler) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response writer wrapper to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		utils.Info("HTTP request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("ip", h.getClientIP(r)),
			zap.Int("status", wrapped.statusCode),
			zap.Duration("duration", time.Since(start)))
	})
}

// Helper functions

// getClientIP extracts the client IP address from the request
func (h *WebSocketHandler) getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header (from load balancer)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// parseJSONRequest parses JSON request body
func (h *WebSocketHandler) parseJSONRequest(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("request body is empty")
	}

	// Limit request body size
	r.Body = http.MaxBytesReader(nil, r.Body, h.maxMessageSize)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	return nil
}

// validateRoomCreateRequest validates room creation request
func (h *WebSocketHandler) validateRoomCreateRequest(req *RoomCreateRequest) error {
	if req.Name == "" {
		return fmt.Errorf("room name is required")
	}
	if len(req.Name) > 100 {
		return fmt.Errorf("room name too long (max 100 characters)")
	}
	if req.CreatedBy == "" {
		return fmt.Errorf("created_by is required")
	}
	if len(req.CreatedBy) > 50 {
		return fmt.Errorf("created_by too long (max 50 characters)")
	}
	if req.MaxUsers < 2 {
		return fmt.Errorf("max_users must be at least 2")
	}
	if req.MaxUsers > 50 {
		return fmt.Errorf("max_users cannot exceed 50")
	}
	return nil
}

// sendJSONResponse sends a JSON response
func (h *WebSocketHandler) sendJSONResponse(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		utils.Error("Failed to encode JSON response", zap.Error(err))
	}
}

// sendErrorResponse sends an error response
func (h *WebSocketHandler) sendErrorResponse(w http.ResponseWriter, statusCode int, message, details string) {
	response := &ErrorResponse{
		Error:   http.StatusText(statusCode),
		Code:    statusCode,
		Message: message,
		Details: details,
	}

	h.sendJSONResponse(w, statusCode, response)
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
