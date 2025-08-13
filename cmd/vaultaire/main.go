package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "go.uber.org/zap"

    "github.com/FairForge/vaultaire/internal/api"
    "github.com/FairForge/vaultaire/internal/config"
)

func main() {
    // Create logger
    logger, _ := zap.NewProduction()
    defer logger.Sync()

    // Create config
    cfg := &config.Config{
        Server: config.ServerConfig{
            Port: 8080,
        },
    }

    // Create server (it creates its own engine internally)
    server := api.NewServer(cfg, logger, nil)

    // Handle shutdown gracefully
    go func() {
        sigChan := make(chan os.Signal, 1)
        signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
        <-sigChan

        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()

        server.Shutdown(ctx)
        os.Exit(0)
    }()

    // Start server
    log.Printf("Starting Vaultaire on port %d", cfg.Server.Port)
    if err := server.Start(); err != nil {
        log.Fatal(err)
    }
}
