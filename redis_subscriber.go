package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RevocationEvent represents a session revocation message from Redis
type RevocationEvent struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	Timestamp string `json:"timestamp"`
}

// RedisSubscriber subscribes to session revocation events from Redis pub/sub
type RedisSubscriber struct {
	client         *redis.Client
	sessionManager *SessionManager
	ctx            context.Context
	cancel         context.CancelFunc
}

// NewRedisSubscriber creates a new Redis subscriber
func NewRedisSubscriber(sessionManager *SessionManager) *RedisSubscriber {
	var opts *redis.Options

	// First check for REDIS_HOST (preferred for Docker/K8s)
	redisHost := os.Getenv("REDIS_HOST")
	redisPort := os.Getenv("REDIS_PORT")

	if redisHost != "" {
		if redisPort == "" {
			redisPort = "6379"
		}
		opts = &redis.Options{
			Addr: redisHost + ":" + redisPort,
			DB:   0,
		}
		log.Printf("[RedisSubscriber] Using REDIS_HOST=%s:%s", redisHost, redisPort)
	} else {
		// Fallback to REDIS_URL
		redisURL := os.Getenv("REDIS_URL")
		if redisURL == "" {
			redisURL = "redis://localhost:6379/0"
		}

		var err error
		opts, err = redis.ParseURL(redisURL)
		if err != nil {
			log.Printf("[RedisSubscriber] Failed to parse REDIS_URL, using localhost")
			opts = &redis.Options{
				Addr: "localhost:6379",
				DB:   0,
			}
		}
		log.Printf("[RedisSubscriber] Using REDIS_URL=%s", redisURL)
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithCancel(context.Background())

	return &RedisSubscriber{
		client:         client,
		sessionManager: sessionManager,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Start begins listening for revocation events
func (rs *RedisSubscriber) Start() error {
	// Test connection
	if err := rs.client.Ping(rs.ctx).Err(); err != nil {
		log.Printf("[RedisSubscriber] Warning: Redis connection failed: %v", err)
		log.Printf("[RedisSubscriber] Revocation events will not be received in real-time")
		return err
	}

	log.Println("[RedisSubscriber] Connected to Redis")

	// Subscribe to revocation pattern
	go rs.subscribeLoop()

	return nil
}

// subscribeLoop handles reconnection and message processing
func (rs *RedisSubscriber) subscribeLoop() {
	for {
		select {
		case <-rs.ctx.Done():
			return
		default:
			rs.subscribe()
			// If subscribe returns, wait before reconnecting
			time.Sleep(5 * time.Second)
		}
	}
}

// subscribe connects to Redis pub/sub and processes messages
func (rs *RedisSubscriber) subscribe() {
	// Subscribe to all revocation channels using pattern
	pubsub := rs.client.PSubscribe(rs.ctx, "stream:revoke:*")
	defer pubsub.Close()

	log.Println("[RedisSubscriber] Subscribed to stream:revoke:* channel pattern")

	ch := pubsub.Channel()

	for {
		select {
		case <-rs.ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				log.Println("[RedisSubscriber] Channel closed, will reconnect")
				return
			}
			rs.handleMessage(msg)
		}
	}
}

// handleMessage processes a revocation message
func (rs *RedisSubscriber) handleMessage(msg *redis.Message) {
	// Extract session_id from channel name (stream:revoke:<session_id>)
	parts := strings.Split(msg.Channel, ":")
	if len(parts) < 3 {
		log.Printf("[RedisSubscriber] Invalid channel format: %s", msg.Channel)
		return
	}
	sessionID := parts[2]

	// Parse message payload
	var event RevocationEvent
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		log.Printf("[RedisSubscriber] Error parsing message: %v", err)
		return
	}

	log.Printf("[RedisSubscriber] Revocation event: session=%s user=%s action=%s",
		sessionID, event.UserID, event.Action)

	// Close the session immediately
	if rs.sessionManager.RevokeSession(sessionID) {
		log.Printf("[RedisSubscriber] Session %s revoked and connections closed", sessionID)
	}
}

// Stop stops the Redis subscriber
func (rs *RedisSubscriber) Stop() {
	rs.cancel()
	rs.client.Close()
	log.Println("[RedisSubscriber] Stopped")
}

// IsConnected checks if Redis is connected
func (rs *RedisSubscriber) IsConnected() bool {
	return rs.client.Ping(rs.ctx).Err() == nil
}
