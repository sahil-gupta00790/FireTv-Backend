package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"fire-tv-rooms-ecs/internal/config"
	"fire-tv-rooms-ecs/internal/models"
	"fire-tv-rooms-ecs/internal/utils"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"go.uber.org/zap"
)

// RoomService manages room operations with DynamoDB persistence and in-memory caching
type RoomService struct {
	// Configuration
	config *config.Config

	// AWS Services
	dynamoDB *dynamodb.DynamoDB

	// In-memory cache for performance
	cache      map[string]*CachedRoom
	cacheMutex sync.RWMutex

	// Redis for distributed caching
	redisService *RedisService

	// Background tasks
	ctx       context.Context
	cancel    context.CancelFunc
	cleanupWG sync.WaitGroup

	// Metrics
	cacheHits   int64
	cacheMisses int64

	// Room expiration
	roomTTL time.Duration
}

// CachedRoom represents a room with caching metadata
type CachedRoom struct {
	Room      *models.Room
	CachedAt  time.Time
	ExpiresAt time.Time
	Dirty     bool // Needs to be written to DynamoDB
}

// RoomFilter represents filtering options for room queries
type RoomFilter struct {
	CreatedBy     string
	IsActive      *bool
	MinUsers      int
	MaxUsers      int
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
}

// RoomSort represents sorting options for room queries
type RoomSort struct {
	Field string // "created_at", "name", "user_count"
	Desc  bool
}

// DynamoDBRoom represents a room as stored in DynamoDB
type DynamoDBRoom struct {
	ID        string                 `dynamodbav:"id"`
	Name      string                 `dynamodbav:"name"`
	CreatedBy string                 `dynamodbav:"created_by"`
	CreatedAt int64                  `dynamodbav:"created_at"`
	UpdatedAt int64                  `dynamodbav:"updated_at"`
	MaxUsers  int                    `dynamodbav:"max_users"`
	IsActive  bool                   `dynamodbav:"is_active"`
	Metadata  map[string]interface{} `dynamodbav:"metadata"`
	TTL       int64                  `dynamodbav:"ttl"`
	GSI1PK    string                 `dynamodbav:"gsi1pk"` // For querying by created_by
	GSI1SK    string                 `dynamodbav:"gsi1sk"` // For sorting by created_at
}

// NewRoomService creates a new room service with DynamoDB and caching
// NewRoomService creates a new room service with DynamoDB and caching
func NewRoomService(cfg *config.Config, redisService *RedisService) (*RoomService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if redisService == nil {
		return nil, fmt.Errorf("redis service cannot be nil")
	}

	// Get environment variables for local development
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	awsCfg := &aws.Config{
		Region: aws.String(cfg.AWS.Region),
	}

	// Configure credentials for local DynamoDB
	if endpoint != "" {
		// Local DynamoDB setup
		awsCfg.Endpoint = aws.String(endpoint)
		awsCfg.Credentials = credentials.NewStaticCredentials(
			"dummyKey123",    // Must be alphanumeric for DynamoDB Local v2.0+
			"dummySecret123", // Must be alphanumeric for DynamoDB Local v2.0+
			"",
		)
		utils.Info("Using local DynamoDB", zap.String("endpoint", endpoint))
	} else {
		// Production AWS setup
		awsCfg.Credentials = credentials.NewStaticCredentials(
			os.Getenv("AWS_ACCESS_KEY_ID"),
			os.Getenv("AWS_SECRET_ACCESS_KEY"),
			"",
		)
	}

	utils.Info("DynamoDB Config",
		zap.String("endpoint", aws.StringValue(awsCfg.Endpoint)),
		zap.String("region", aws.StringValue(awsCfg.Region)),
	)

	sess, err := session.NewSession(awsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %w", err)
	}

	// ... rest of the function

	ctx, cancel := context.WithCancel(context.Background())

	rs := &RoomService{
		config:       cfg,
		dynamoDB:     dynamodb.New(sess),
		cache:        make(map[string]*CachedRoom),
		redisService: redisService,
		ctx:          ctx,
		cancel:       cancel,
		roomTTL:      cfg.App.RoomTTL,
	}

	// Start background tasks
	rs.startBackgroundTasks()

	utils.Info("Room service initialized",
		zap.String("dynamodb_table", cfg.AWS.DynamoDBTable),
		zap.Duration("room_ttl", rs.roomTTL))

	return rs, nil
}

// CreateRoom creates a new room with validation and persistence
func (rs *RoomService) CreateRoom(room *models.Room) error {
	if room == nil {
		return fmt.Errorf("room cannot be nil")
	}

	// Validate room
	if err := rs.validateRoom(room); err != nil {
		return fmt.Errorf("room validation failed: %w", err)
	}

	// Check if room already exists
	if _, err := rs.GetRoom(room.ID); err == nil {
		return fmt.Errorf("room already exists: %s", room.ID)
	}

	// Set timestamps
	now := time.Now()
	room.CreatedAt = now
	room.UpdatedAt = now

	// Convert to DynamoDB format
	dynamoRoom := rs.roomToDynamoDB(room)

	// Save to DynamoDB
	item, err := dynamodbattribute.MarshalMap(dynamoRoom)
	if err != nil {
		return fmt.Errorf("failed to marshal room: %w", err)
	}

	input := &dynamodb.PutItemInput{
		TableName:           aws.String(rs.config.AWS.DynamoDBTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(id)"),
	}

	_, err = rs.dynamoDB.PutItem(input)
	if err != nil {
		utils.Error("Failed to create room in DynamoDB",
			zap.Error(err),
			zap.String("room_id", room.ID))
		return fmt.Errorf("failed to create room: %w", err)
	}

	// Cache the room
	rs.cacheRoom(room)

	// Publish room creation event to Redis
	rs.publishRoomEvent("room_created", room)

	utils.Info("Room created successfully",
		zap.String("room_id", room.ID),
		zap.String("name", room.Name),
		zap.String("created_by", room.CreatedBy),
		zap.Int("max_users", room.MaxUsers))

	return nil
}

// GetRoom retrieves a room by ID with caching
func (rs *RoomService) GetRoom(roomID string) (*models.Room, error) {
	if roomID == "" {
		return nil, fmt.Errorf("room ID cannot be empty")
	}

	// Check cache first
	if cachedRoom := rs.getCachedRoom(roomID); cachedRoom != nil {
		rs.cacheHits++
		return cachedRoom, nil
	}

	rs.cacheMisses++

	// Get from DynamoDB
	input := &dynamodb.GetItemInput{
		TableName: aws.String(rs.config.AWS.DynamoDBTable),
		Key: map[string]*dynamodb.AttributeValue{
			"id": {
				S: aws.String(roomID),
			},
		},
	}

	result, err := rs.dynamoDB.GetItem(input)
	if err != nil {
		utils.Error("Failed to get room from DynamoDB",
			zap.Error(err),
			zap.String("room_id", roomID))
		return nil, fmt.Errorf("failed to get room: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("room not found: %s", roomID)
	}

	// Unmarshal DynamoDB item
	var dynamoRoom DynamoDBRoom
	if err := dynamodbattribute.UnmarshalMap(result.Item, &dynamoRoom); err != nil {
		return nil, fmt.Errorf("failed to unmarshal room: %w", err)
	}

	room := rs.dynamoDBToRoom(&dynamoRoom)

	// Cache the room
	rs.cacheRoom(room)

	return room, nil
}

// UpdateRoom updates an existing room
func (rs *RoomService) UpdateRoom(room *models.Room) error {
	if room == nil {
		return fmt.Errorf("room cannot be nil")
	}
	if room.ID == "" {
		return fmt.Errorf("room ID cannot be empty")
	}

	// Validate room
	if err := rs.validateRoom(room); err != nil {
		return fmt.Errorf("room validation failed: %w", err)
	}

	// Check if room exists
	existingRoom, err := rs.GetRoom(room.ID)
	if err != nil {
		return fmt.Errorf("room not found: %w", err)
	}

	// Update timestamps
	room.CreatedAt = existingRoom.CreatedAt // Preserve creation time
	room.UpdatedAt = time.Now()

	// Convert to DynamoDB format
	dynamoRoom := rs.roomToDynamoDB(room)

	// Update in DynamoDB
	item, err := dynamodbattribute.MarshalMap(dynamoRoom)
	if err != nil {
		return fmt.Errorf("failed to marshal room: %w", err)
	}

	input := &dynamodb.PutItemInput{
		TableName:           aws.String(rs.config.AWS.DynamoDBTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_exists(id)"),
	}

	_, err = rs.dynamoDB.PutItem(input)
	if err != nil {
		utils.Error("Failed to update room in DynamoDB",
			zap.Error(err),
			zap.String("room_id", room.ID))
		return fmt.Errorf("failed to update room: %w", err)
	}

	// Update cache
	rs.cacheRoom(room)

	// Publish room update event to Redis
	rs.publishRoomEvent("room_updated", room)

	utils.Info("Room updated successfully",
		zap.String("room_id", room.ID),
		zap.String("name", room.Name))

	return nil
}

// DeleteRoom deletes a room by ID
func (rs *RoomService) DeleteRoom(roomID string) error {
	if roomID == "" {
		return fmt.Errorf("room ID cannot be empty")
	}

	// Get room first to verify it exists
	room, err := rs.GetRoom(roomID)
	if err != nil {
		return fmt.Errorf("room not found: %w", err)
	}

	// Delete from DynamoDB
	input := &dynamodb.DeleteItemInput{
		TableName: aws.String(rs.config.AWS.DynamoDBTable),
		Key: map[string]*dynamodb.AttributeValue{
			"id": {
				S: aws.String(roomID),
			},
		},
		ConditionExpression: aws.String("attribute_exists(id)"),
	}

	_, err = rs.dynamoDB.DeleteItem(input)
	if err != nil {
		utils.Error("Failed to delete room from DynamoDB",
			zap.Error(err),
			zap.String("room_id", roomID))
		return fmt.Errorf("failed to delete room: %w", err)
	}

	// Remove from cache
	rs.removeCachedRoom(roomID)

	// Publish room deletion event to Redis
	rs.publishRoomEvent("room_deleted", room)

	utils.Info("Room deleted successfully",
		zap.String("room_id", roomID),
		zap.String("name", room.Name))

	return nil
}

// ListRooms returns a list of rooms with filtering, sorting, and pagination
func (rs *RoomService) ListRooms(limit, offset int, filter *RoomFilter, sort *RoomSort) ([]*models.Room, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	// For simplicity, we'll scan all rooms and filter/sort in memory
	// In production, you'd use DynamoDB GSIs for efficient querying
	rooms, err := rs.scanAllRooms()
	if err != nil {
		return nil, fmt.Errorf("failed to scan rooms: %w", err)
	}

	// Apply filters
	if filter != nil {
		rooms = rs.filterRooms(rooms, filter)
	}

	// Apply sorting
	if sort != nil {
		rs.sortRooms(rooms, sort)
	}

	// Apply pagination
	if offset >= len(rooms) {
		return []*models.Room{}, nil
	}

	end := offset + limit
	if end > len(rooms) {
		end = len(rooms)
	}

	return rooms[offset:end], nil
}

// GetRoomsByCreator returns rooms created by a specific user
func (rs *RoomService) GetRoomsByCreator(createdBy string, limit int) ([]*models.Room, error) {
	if createdBy == "" {
		return nil, fmt.Errorf("created_by cannot be empty")
	}

	// Use GSI to query by created_by
	input := &dynamodb.QueryInput{
		TableName:              aws.String(rs.config.AWS.DynamoDBTable),
		IndexName:              aws.String("GSI1"),
		KeyConditionExpression: aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":pk": {
				S: aws.String(fmt.Sprintf("USER#%s", createdBy)),
			},
		},
		ScanIndexForward: aws.Bool(false), // Sort by created_at descending
		Limit:            aws.Int64(int64(limit)),
	}

	result, err := rs.dynamoDB.Query(input)
	if err != nil {
		utils.Error("Failed to query rooms by creator",
			zap.Error(err),
			zap.String("created_by", createdBy))
		return nil, fmt.Errorf("failed to query rooms: %w", err)
	}

	var rooms []*models.Room
	for _, item := range result.Items {
		var dynamoRoom DynamoDBRoom
		if err := dynamodbattribute.UnmarshalMap(item, &dynamoRoom); err != nil {
			utils.Error("Failed to unmarshal room",
				zap.Error(err))
			continue
		}
		rooms = append(rooms, rs.dynamoDBToRoom(&dynamoRoom))
	}

	return rooms, nil
}

// DeactivateRoom marks a room as inactive
func (rs *RoomService) DeactivateRoom(roomID string) error {
	room, err := rs.GetRoom(roomID)
	if err != nil {
		return err
	}

	room.IsActive = false
	return rs.UpdateRoom(room)
}

// ActivateRoom marks a room as active
func (rs *RoomService) ActivateRoom(roomID string) error {
	room, err := rs.GetRoom(roomID)
	if err != nil {
		return err
	}

	room.IsActive = true
	return rs.UpdateRoom(room)
}

// GetActiveRoomsCount returns the count of active rooms
func (rs *RoomService) GetActiveRoomsCount() (int, error) {
	rooms, err := rs.scanAllRooms()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, room := range rooms {
		if room.IsActive {
			count++
		}
	}

	return count, nil
}

// Cache management methods

// getCachedRoom retrieves a room from cache if valid
func (rs *RoomService) getCachedRoom(roomID string) *models.Room {
	rs.cacheMutex.RLock()
	defer rs.cacheMutex.RUnlock()

	cached, exists := rs.cache[roomID]
	if !exists {
		return nil
	}

	// Check if cache entry is expired
	if time.Now().After(cached.ExpiresAt) {
		return nil
	}

	return cached.Room
}

// cacheRoom stores a room in cache
func (rs *RoomService) cacheRoom(room *models.Room) {
	rs.cacheMutex.Lock()
	defer rs.cacheMutex.Unlock()

	rs.cache[room.ID] = &CachedRoom{
		Room:      room,
		CachedAt:  time.Now(),
		ExpiresAt: time.Now().Add(time.Minute * 15), // 15 minute cache TTL
		Dirty:     false,
	}
}

// removeCachedRoom removes a room from cache
func (rs *RoomService) removeCachedRoom(roomID string) {
	rs.cacheMutex.Lock()
	defer rs.cacheMutex.Unlock()

	delete(rs.cache, roomID)
}

// Helper methods

// validateRoom validates room data
func (rs *RoomService) validateRoom(room *models.Room) error {
	if room.ID == "" {
		return fmt.Errorf("room ID cannot be empty")
	}
	if room.Name == "" {
		return fmt.Errorf("room name cannot be empty")
	}
	if len(room.Name) > 100 {
		return fmt.Errorf("room name too long (max 100 characters)")
	}
	if room.CreatedBy == "" {
		return fmt.Errorf("created_by cannot be empty")
	}
	if len(room.CreatedBy) > 50 {
		return fmt.Errorf("created_by too long (max 50 characters)")
	}
	if room.MaxUsers < 2 {
		return fmt.Errorf("max_users must be at least 2")
	}
	if room.MaxUsers > 50 {
		return fmt.Errorf("max_users cannot exceed 50")
	}
	return nil
}

// roomToDynamoDB converts a Room to DynamoDB format
func (rs *RoomService) roomToDynamoDB(room *models.Room) *DynamoDBRoom {
	return &DynamoDBRoom{
		ID:        room.ID,
		Name:      room.Name,
		CreatedBy: room.CreatedBy,
		CreatedAt: room.CreatedAt.Unix(),
		UpdatedAt: room.UpdatedAt.Unix(),
		MaxUsers:  room.MaxUsers,
		IsActive:  room.IsActive,
		Metadata:  room.Metadata,
		TTL:       time.Now().Add(rs.roomTTL).Unix(),
		GSI1PK:    fmt.Sprintf("USER#%s", room.CreatedBy),
		GSI1SK:    fmt.Sprintf("ROOM#%d", room.CreatedAt.Unix()),
	}
}

// dynamoDBToRoom converts DynamoDB format to Room
func (rs *RoomService) dynamoDBToRoom(dynamoRoom *DynamoDBRoom) *models.Room {
	return &models.Room{
		ID:        dynamoRoom.ID,
		Name:      dynamoRoom.Name,
		CreatedBy: dynamoRoom.CreatedBy,
		CreatedAt: time.Unix(dynamoRoom.CreatedAt, 0),
		UpdatedAt: time.Unix(dynamoRoom.UpdatedAt, 0),
		MaxUsers:  dynamoRoom.MaxUsers,
		IsActive:  dynamoRoom.IsActive,
		Metadata:  dynamoRoom.Metadata,
		Users:     make([]models.User, 0), // Users are managed separately
	}
}

// scanAllRooms scans all rooms from DynamoDB (for internal use)
func (rs *RoomService) scanAllRooms() ([]*models.Room, error) {
	input := &dynamodb.ScanInput{
		TableName: aws.String(rs.config.AWS.DynamoDBTable),
	}

	var rooms []*models.Room

	err := rs.dynamoDB.ScanPages(input, func(page *dynamodb.ScanOutput, lastPage bool) bool {
		for _, item := range page.Items {
			var dynamoRoom DynamoDBRoom
			if err := dynamodbattribute.UnmarshalMap(item, &dynamoRoom); err != nil {
				utils.Error("Failed to unmarshal room during scan",
					zap.Error(err))
				continue
			}
			rooms = append(rooms, rs.dynamoDBToRoom(&dynamoRoom))
		}
		return !lastPage
	})

	if err != nil {
		return nil, fmt.Errorf("failed to scan rooms: %w", err)
	}

	return rooms, nil
}

// filterRooms applies filters to a list of rooms
func (rs *RoomService) filterRooms(rooms []*models.Room, filter *RoomFilter) []*models.Room {
	var filtered []*models.Room

	for _, room := range rooms {
		if filter.CreatedBy != "" && room.CreatedBy != filter.CreatedBy {
			continue
		}
		if filter.IsActive != nil && room.IsActive != *filter.IsActive {
			continue
		}
		if filter.MinUsers > 0 && room.MaxUsers < filter.MinUsers {
			continue
		}
		if filter.MaxUsers > 0 && room.MaxUsers > filter.MaxUsers {
			continue
		}
		if filter.CreatedAfter != nil && room.CreatedAt.Before(*filter.CreatedAfter) {
			continue
		}
		if filter.CreatedBefore != nil && room.CreatedAt.After(*filter.CreatedBefore) {
			continue
		}
		filtered = append(filtered, room)
	}

	return filtered
}

// sortRooms sorts a list of rooms
func (rs *RoomService) sortRooms(rooms []*models.Room, sortOpt *RoomSort) {
	sort.Slice(rooms, func(i, j int) bool {
		var less bool
		switch sortOpt.Field {
		case "name":
			less = rooms[i].Name < rooms[j].Name
		case "created_at":
			less = rooms[i].CreatedAt.Before(rooms[j].CreatedAt)
		case "user_count":
			// This would require additional data about current users
			less = rooms[i].MaxUsers < rooms[j].MaxUsers
		default:
			less = rooms[i].CreatedAt.Before(rooms[j].CreatedAt)
		}

		if sortOpt.Desc {
			return !less
		}
		return less
	})
}

// publishRoomEvent publishes room events to Redis
func (rs *RoomService) publishRoomEvent(eventType string, room *models.Room) {
	event := map[string]interface{}{
		"type":      eventType,
		"room_id":   room.ID,
		"room_name": room.Name,
		"timestamp": time.Now(),
	}

	data, err := json.Marshal(event)
	if err != nil {
		utils.Error("Failed to marshal room event",
			zap.Error(err),
			zap.String("event_type", eventType))
		return
	}

	// Publish to Redis (fire and forget)
	go func() {
		_, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()

		if err := rs.redisService.PublishMessage("room_events", &models.WebSocketMessage{
			Type:      models.MessageType(eventType),
			RoomID:    room.ID,
			Data:      map[string]interface{}{"event": string(data)},
			Timestamp: time.Now(),
			MessageID: room.ID,
		}); err != nil {
			utils.Error("Failed to publish room event to Redis",
				zap.Error(err),
				zap.String("event_type", eventType))
		}
	}()
}

// Background tasks

// startBackgroundTasks starts background maintenance tasks
func (rs *RoomService) startBackgroundTasks() {
	rs.cleanupWG.Add(2)
	go rs.cacheCleanupTask()
	go rs.metricsReportingTask()
}

// cacheCleanupTask periodically cleans up expired cache entries
func (rs *RoomService) cacheCleanupTask() {
	defer rs.cleanupWG.Done()

	ticker := time.NewTicker(time.Minute * 5)
	defer ticker.Stop()

	for {
		select {
		case <-rs.ctx.Done():
			return
		case <-ticker.C:
			rs.cleanupExpiredCache()
		}
	}
}

// cleanupExpiredCache removes expired entries from cache
func (rs *RoomService) cleanupExpiredCache() {
	rs.cacheMutex.Lock()
	defer rs.cacheMutex.Unlock()

	now := time.Now()
	expired := make([]string, 0)

	for roomID, cached := range rs.cache {
		if now.After(cached.ExpiresAt) {
			expired = append(expired, roomID)
		}
	}

	for _, roomID := range expired {
		delete(rs.cache, roomID)
	}

	if len(expired) > 0 {
		utils.Debug("Cleaned up expired cache entries",
			zap.Int("count", len(expired)))
	}
}

// metricsReportingTask periodically reports cache metrics
func (rs *RoomService) metricsReportingTask() {
	defer rs.cleanupWG.Done()

	ticker := time.NewTicker(time.Minute * 5)
	defer ticker.Stop()

	for {
		select {
		case <-rs.ctx.Done():
			return
		case <-ticker.C:
			rs.reportCacheMetrics()
		}
	}
}

// reportCacheMetrics reports cache hit/miss metrics
func (rs *RoomService) reportCacheMetrics() {
	rs.cacheMutex.RLock()
	cacheSize := len(rs.cache)
	rs.cacheMutex.RUnlock()

	utils.Info("Room service cache metrics",
		zap.Int("cache_size", cacheSize),
		zap.Int64("cache_hits", rs.cacheHits),
		zap.Int64("cache_misses", rs.cacheMisses))
}

// HealthCheck performs a health check on the room service
func (rs *RoomService) HealthCheck() error {
	// Test DynamoDB connectivity
	input := &dynamodb.DescribeTableInput{
		TableName: aws.String(rs.config.AWS.DynamoDBTable),
	}

	ctx, cancel := context.WithTimeout(rs.ctx, time.Second*5)
	defer cancel()

	_, err := rs.dynamoDB.DescribeTableWithContext(ctx, input)
	if err != nil {
		return fmt.Errorf("DynamoDB health check failed: %w", err)
	}

	return nil
}

// Close gracefully shuts down the room service
func (rs *RoomService) Close() error {
	utils.Info("Shutting down room service...")

	// Cancel context to stop background tasks
	rs.cancel()

	// Wait for background tasks to finish
	done := make(chan struct{})
	go func() {
		rs.cleanupWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		utils.Info("Room service shut down successfully")
	case <-time.After(time.Second * 30):
		utils.Warn("Room service shutdown timeout exceeded")
	}

	return nil
}
