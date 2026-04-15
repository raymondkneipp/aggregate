package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/chi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/handler"
	"github.com/raymondkneipp/aggregate/internal/repository"
)

func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		startLambda()
	} else {
		startLocal()
	}
}

func buildRouter(cfg *config.Config) (http.Handler, error) {
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	repo := repository.NewJobRepository(db)
	jobHandler := handler.NewJobHandler(repo)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	r.Mount("/jobs", jobHandler.Routes())
	return r, nil
}

func startLambda() {
	cfg := config.Load()
	router, err := buildRouter(cfg)
	if err != nil {
		slog.Error("setup router", "error", err)
		os.Exit(1)
	}
	adapter := chiadapter.New(router.(*chi.Mux))
	lambda.Start(adapter.ProxyWithContext)
}

func startLocal() {
	cfg := config.Load()
	router, err := buildRouter(cfg)
	if err != nil {
		slog.Error("setup router", "error", err)
		os.Exit(1)
	}
	slog.Info("listening", "addr", ":8080")
	http.ListenAndServe(":8080", router)
}
