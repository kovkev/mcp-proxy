package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/errgroup"
)

type MiddlewareFunc func(http.Handler) http.Handler

func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, mw := range middlewares {
		h = mw(h)
	}
	return h
}

func newAuthMiddleware(tokens []string) MiddlewareFunc {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		tokenSet[token] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(tokens) != 0 {
				token := r.Header.Get("Authorization")
				token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
				if token == "" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if _, ok := tokenSet[token]; !ok {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func loggerMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("<%s> Request [%s] %s", prefix, r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
}

func recoverMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					log.Printf("<%s> Recovered from panic: %v", prefix, err)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func startHTTPServer(config *Config) error {
	baseURL, uErr := url.Parse(config.McpProxy.BaseURL)
	if uErr != nil {
		return uErr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errorGroup errgroup.Group
	httpMux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:    config.McpProxy.Addr,
		Handler: httpMux,
	}
	info := mcp.Implementation{
		Name: config.McpProxy.Name,
	}

	// Create a single unified MCP server that aggregates all backend servers
	unifiedServer, err := newMCPServer("unified", config.McpProxy, &MCPClientConfigV2{
		Options: &OptionsV2{
			LogEnabled: config.McpProxy.Options.LogEnabled,
		},
	})
	if err != nil {
		return err
	}

	// Collect all auth tokens from all servers for unified auth
	allAuthTokens := make([]string, 0)
	tokenSet := make(map[string]struct{})

	for name, clientConfig := range config.McpServers {
		mcpClient, err := newMCPClient(name, clientConfig)
		if err != nil {
			return err
		}

		// Collect auth tokens
		if clientConfig.Options != nil && len(clientConfig.Options.AuthTokens) > 0 {
			for _, token := range clientConfig.Options.AuthTokens {
				if _, exists := tokenSet[token]; !exists {
					tokenSet[token] = struct{}{}
					allAuthTokens = append(allAuthTokens, token)
				}
			}
		}

		errorGroup.Go(func() error {
			log.Printf("<%s> Connecting", name)
			addErr := mcpClient.addToMCPServer(ctx, info, unifiedServer.mcpServer)
			if addErr != nil {
				log.Printf("<%s> Failed to add client to server: %v", name, addErr)
				if clientConfig.Options.PanicIfInvalid.OrElse(false) {
					return addErr
				}
				return nil
			}
			log.Printf("<%s> Connected and added to unified server", name)

			httpServer.RegisterOnShutdown(func() {
				log.Printf("<%s> Shutting down", name)
				_ = mcpClient.Close()
			})
			return nil
		})
	}

	// Register the unified server at the root path
	go func() {
		err := errorGroup.Wait()
		if err != nil {
			log.Fatalf("Failed to add clients: %v", err)
		}
		log.Printf("All clients initialized and added to unified MCP server")

		// Setup middlewares for the unified server
		middlewares := make([]MiddlewareFunc, 0)
		middlewares = append(middlewares, recoverMiddleware("unified"))
		if config.McpProxy.Options.LogEnabled.OrElse(false) {
			middlewares = append(middlewares, loggerMiddleware("unified"))
		}
		if len(allAuthTokens) > 0 {
			middlewares = append(middlewares, newAuthMiddleware(allAuthTokens))
		}

		mcpRoute := baseURL.Path
		if mcpRoute == "" {
			mcpRoute = "/"
		}
		if !strings.HasPrefix(mcpRoute, "/") {
			mcpRoute = "/" + mcpRoute
		}
		if !strings.HasSuffix(mcpRoute, "/") {
			mcpRoute += "/"
		}

		log.Printf("Unified MCP server handling all requests at %s", mcpRoute)
		httpMux.Handle(mcpRoute, chainMiddleware(unifiedServer.handler, middlewares...))
	}()

	go func() {
		log.Printf("Starting %s server", config.McpProxy.Type)
		log.Printf("%s server listening on %s", config.McpProxy.Type, config.McpProxy.Addr)
		hErr := httpServer.ListenAndServe()
		if hErr != nil && !errors.Is(hErr, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", hErr)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
		return shutdownErr
	}
	return nil
}
