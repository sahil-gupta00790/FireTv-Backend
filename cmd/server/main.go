package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"fire-tv-rooms-ecs/internal/config"
	"fire-tv-rooms-ecs/internal/handlers"
	"fire-tv-rooms-ecs/internal/services"
	"fire-tv-rooms-ecs/internal/utils"

	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

var (
	// Build information (set during build)
	Version   = "1.0.0"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func main() {
	// Load environment variables from .env file if it exists
	if err := godotenv.Load(); err != nil {
		// Ignore error if .env file doesn't exist (production deployment)
		fmt.Printf("No .env file found, using environment variables\n")
	}

	// Load configuration
	cfg := config.Load()

	// Initialize logger first
	if err := utils.InitLogger(cfg.App.LogLevel, cfg.App.Environment); err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	// Log startup information
	utils.Info("Starting Fire TV Rooms server",
		zap.String("version", Version),
		zap.String("build_time", BuildTime),
		zap.String("git_commit", GitCommit),
		zap.String("environment", cfg.App.Environment),
		zap.String("go_version", runtime.Version()),
		zap.Int("cpu_count", runtime.NumCPU()),
		zap.String("server_address", fmt.Sprintf("%s:%s", cfg.Server.Host, cfg.Server.Port)))

	// Set GOMAXPROCS for container environments
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Initialize services with proper error handling and cleanup
	app, err := initializeApplication(cfg)
	if err != nil {
		utils.Fatal("Failed to initialize application", zap.Error(err))
	}

	// Start HTTP server
	server := startHTTPServer(app)

	// Setup graceful shutdown
	setupGracefulShutdown(server, app, cfg)

	utils.Info("Fire TV Rooms server started successfully")
}

// Application holds all application services and dependencies
type Application struct {
	Config            *config.Config
	RedisService      *services.RedisService
	RoomService       *services.RoomService
	ConnectionManager *services.ConnectionManager
	WebSocketHandler  *handlers.WebSocketHandler
}

// initializeApplication initializes all application services
func initializeApplication(cfg *config.Config) (*Application, error) {
	app := &Application{
		Config: cfg,
	}

	// Initialize Redis service
	utils.Info("Initializing Redis service...")
	redisService, err := services.NewRedisService(&cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Redis service: %w", err)
	}
	app.RedisService = redisService

	// Test Redis connectivity
	if err := redisService.HealthCheck(); err != nil {
		redisService.Close()
		return nil, fmt.Errorf("Redis health check failed: %w", err)
	}
	utils.Info("Redis service initialized successfully")

	// Initialize Room service
	utils.Info("Initializing Room service...")
	roomService, err := services.NewRoomService(cfg, redisService)
	if err != nil {
		redisService.Close()
		return nil, fmt.Errorf("failed to initialize Room service: %w", err)
	}
	app.RoomService = roomService

	// Test Room service connectivity
	if err := roomService.HealthCheck(); err != nil {
		roomService.Close()
		redisService.Close()
		return nil, fmt.Errorf("Room service health check failed: %w", err)
	}
	utils.Info("Room service initialized successfully")

	// Initialize Connection Manager
	utils.Info("Initializing Connection Manager...")
	connectionManager, err := services.NewConnectionManager(cfg, redisService)
	if err != nil {
		roomService.Close()
		redisService.Close()
		return nil, fmt.Errorf("failed to initialize Connection Manager: %w", err)
	}
	app.ConnectionManager = connectionManager

	// Test Connection Manager
	if err := connectionManager.HealthCheck(); err != nil {
		connectionManager.Close()
		roomService.Close()
		redisService.Close()
		return nil, fmt.Errorf("Connection Manager health check failed: %w", err)
	}
	utils.Info("Connection Manager initialized successfully")

	// Initialize WebSocket handler
	utils.Info("Initializing WebSocket handler...")
	wsHandler, err := handlers.NewWebSocketHandler(connectionManager, redisService, roomService, cfg)
	if err != nil {
		connectionManager.Close()
		roomService.Close()
		redisService.Close()
		return nil, fmt.Errorf("failed to initialize WebSocket handler: %w", err)
	}
	app.WebSocketHandler = wsHandler
	utils.Info("WebSocket handler initialized successfully")

	utils.Info("All services initialized successfully")
	return app, nil
}

// startHTTPServer starts the HTTP server with proper configuration
func startHTTPServer(app *Application) *http.Server {
	router := setupRouter(app)

	// --- 1️⃣  Autocert manager ------------------------------------------
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache("certs"), // <-- persisted via volume
		HostPolicy: autocert.HostWhitelist("firetcbackend.zapto.org"),
	}

	// --- 2️⃣  HTTPS server on :443 --------------------------------------
	server := &http.Server{
		Addr:         ":443",
		Handler:      router,
		ReadTimeout:  app.Config.Server.ReadTimeout,
		WriteTimeout: app.Config.Server.WriteTimeout,
		IdleTimeout:  time.Second * 120,
		TLSConfig: &tls.Config{
			GetCertificate: m.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		},
	}

	// --- 3️⃣  HTTP-01 challenge + redirect 80->443 ----------------------
	go func() {
		//  Serve ACME challenge and redirect everything else to HTTPS
		httpHandler := m.HTTPHandler(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
			}))
		if err := http.ListenAndServe(":80", httpHandler); err != nil {
			utils.Fatal("HTTP-01 listener failed", zap.Error(err))
		}
	}()

	// --- 4️⃣  Start HTTPS server ----------------------------------------
	go func() {
		utils.Info("HTTPS server starting", zap.String("address", server.Addr))
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			utils.Fatal("HTTPS server failed", zap.Error(err))
		}
	}()

	return server

}

// setupRouter configures the HTTP router with all routes and middleware
func setupRouter(app *Application) *mux.Router {
	router := mux.NewRouter()

	// Add global middleware
	router.Use(recoveryMiddleware)
	router.Use(requestIDMiddleware)
	router.Use(securityHeadersMiddleware)

	// Register WebSocket handler routes
	app.WebSocketHandler.RegisterRoutes(router)

	// Add system routes
	router.HandleFunc("/", rootHandler).Methods("GET")
	router.HandleFunc("/version", versionHandler).Methods("GET")
	router.HandleFunc("/metrics", metricsHandler(app)).Methods("GET")

	// Add 404 handler
	router.NotFoundHandler = http.HandlerFunc(notFoundHandler)

	return router
}

// setupGracefulShutdown configures graceful shutdown handling
func setupGracefulShutdown(server *http.Server, app *Application, cfg *config.Config) {
	// Create channel to listen for interrupt signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Block until we receive a signal
	sig := <-quit
	utils.Info("Shutdown signal received",
		zap.String("signal", sig.String()))

	// Create context with timeout for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	// Shutdown HTTP server
	utils.Info("Shutting down HTTP server...")
	if err := server.Shutdown(ctx); err != nil {
		utils.Error("HTTP server forced to shutdown", zap.Error(err))
	} else {
		utils.Info("HTTP server shut down gracefully")
	}

	// Close application services in reverse order
	closeApplication(app)

	utils.Info("Fire TV Rooms server shut down successfully")
}

// closeApplication closes all application services gracefully
func closeApplication(app *Application) {
	if app == nil {
		return
	}

	// Close services in reverse order of initialization
	if app.ConnectionManager != nil {
		utils.Info("Closing Connection Manager...")
		if err := app.ConnectionManager.Close(); err != nil {
			utils.Error("Failed to close Connection Manager", zap.Error(err))
		} else {
			utils.Info("Connection Manager closed successfully")
		}
	}

	if app.RoomService != nil {
		utils.Info("Closing Room Service...")
		if err := app.RoomService.Close(); err != nil {
			utils.Error("Failed to close Room Service", zap.Error(err))
		} else {
			utils.Info("Room Service closed successfully")
		}
	}

	if app.RedisService != nil {
		utils.Info("Closing Redis Service...")
		if err := app.RedisService.Close(); err != nil {
			utils.Error("Failed to close Redis Service", zap.Error(err))
		} else {
			utils.Info("Redis Service closed successfully")
		}
	}
}

// HTTP Handlers

// rootHandler handles requests to the root path
func rootHandler(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"service":   "Fire TV Rooms",
		"version":   Version,
		"status":    "running",
		"timestamp": time.Now().UTC(),
		"endpoints": map[string]string{
			"websocket": "/ws",
			"health":    "/health",
			"ready":     "/ready",
			"version":   "/version",
			"metrics":   "/metrics",
			"api":       "/api/v1",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := writeJSONResponse(w, response); err != nil {
		utils.Error("Failed to write root response", zap.Error(err))
	}
}

// versionHandler handles version requests
func versionHandler(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"version":    Version,
		"build_time": BuildTime,
		"git_commit": GitCommit,
		"go_version": runtime.Version(),
		"timestamp":  time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := writeJSONResponse(w, response); err != nil {
		utils.Error("Failed to write version response", zap.Error(err))
	}
}

// metricsHandler handles metrics requests
func metricsHandler(app *Application) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		response := map[string]interface{}{
			"timestamp": time.Now().UTC(),
			"system": map[string]interface{}{
				"goroutines":   runtime.NumGoroutine(),
				"cpu_count":    runtime.NumCPU(),
				"memory_alloc": memStats.Alloc,
				"memory_total": memStats.TotalAlloc,
				"memory_sys":   memStats.Sys,
				"gc_runs":      memStats.NumGC,
			},
			"connections": map[string]interface{}{
				"active": app.ConnectionManager.GetConnectionCount(),
				"max":    app.Config.Server.MaxConnections,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if err := writeJSONResponse(w, response); err != nil {
			utils.Error("Failed to write metrics response", zap.Error(err))
		}
	}
}

// notFoundHandler handles 404 errors
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"error":     "Not Found",
		"message":   "The requested resource was not found",
		"path":      r.URL.Path,
		"method":    r.Method,
		"timestamp": time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)

	if err := writeJSONResponse(w, response); err != nil {
		utils.Error("Failed to write 404 response", zap.Error(err))
	}
}

// Middleware

// recoveryMiddleware recovers from panics and logs them
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				utils.Error("Panic recovered in HTTP handler",
					zap.Any("error", err),
					zap.String("path", r.URL.Path),
					zap.String("method", r.Method),
					zap.String("remote_addr", r.RemoteAddr))

				// Return 500 Internal Server Error
				response := map[string]interface{}{
					"error":     "Internal Server Error",
					"message":   "An unexpected error occurred",
					"timestamp": time.Now().UTC(),
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				writeJSONResponse(w, response)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware adds a unique request ID to each request
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := generateRequestID()

		// Add request ID to response headers
		w.Header().Set("X-Request-ID", requestID)

		// Add request ID to context for use in handlers
		ctx := context.WithValue(r.Context(), "request_id", requestID)
		r = r.WithContext(ctx)

		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware adds security headers to responses
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Only add HSTS in production with HTTPS
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// Utility functions

// writeJSONResponse writes a JSON response to the ResponseWriter
func writeJSONResponse(w http.ResponseWriter, data interface{}) error {
	w.Header().Set("Content-Type", "application/json")

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	return encoder.Encode(data)
}

// generateRequestID generates a unique request ID
func generateRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}
