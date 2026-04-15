package config

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

type Config struct {
	JinaAPIKey      string
	AnthropicAPIKey string
	DatabaseURL     string
	SQSQueueURL     string
	Environment     string
	Sites           []string
	Roles           []string
}

func Load() *Config {
	env := getEnv("ENVIRONMENT", "local")

	cfg := &Config{
		Environment: env,
		Sites:       splitEnv("SITES", "jobs.lever.co"),
		Roles:       splitEnv("ROLES", "software engineer"),
		// Sites:           splitEnv("SITES", "jobs.lever.co,boards.greenhouse.io,jobs.ashby.io,apply.workable.com"),
		// Roles:           splitEnv("ROLES", "software engineer,software developer,frontend developer,full stack developer"),

	}

	if env == "local" {
		cfg.JinaAPIKey = mustEnv("JINA_API_KEY")
		cfg.AnthropicAPIKey = mustEnv("ANTHROPIC_API_KEY")
		cfg.DatabaseURL = mustEnv("DATABASE_URL")
		cfg.SQSQueueURL = os.Getenv("SQS_QUEUE_URL")
	} else {
		cfg.JinaAPIKey = getSSM(env, "jina_key")
		cfg.AnthropicAPIKey = getSSM(env, "anthropic_key")
		cfg.DatabaseURL = getSSM(env, "db_url")
		cfg.SQSQueueURL = getSSM(env, "sqs_url")
	}

	return cfg
}

func getSSM(env, name string) string {
	path := "/" + env + "/aggregate/" + name
	awscfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}
	client := ssm.NewFromConfig(awscfg)
	resp, err := client.GetParameter(context.Background(), &ssm.GetParameterInput{
		Name:           aws.String(path),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		log.Fatalf("get SSM param %s: %v", path, err)
	}
	return *resp.Parameter.Value
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitEnv(key, fallback string) []string {
	v := getEnv(key, fallback)
	return strings.Split(v, ",")
}
