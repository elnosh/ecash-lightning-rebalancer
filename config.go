package main

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Env struct {
	grpcHost     string
	tlsCertPath  string
	macaroonPath string
	walletPath   string
	mint         string
}

func loadEnv() (*Env, error) {
	if err := godotenv.Load(); err != nil {
		return nil, fmt.Errorf("could not load .env file: %v", err)
	}

	host := os.Getenv("LND_GRPC_HOST")
	if host == "" {
		return nil, fmt.Errorf("LND_GRPC_HOST cannot be empty")
	}
	certPath := os.Getenv("LND_CERT_PATH")
	if certPath == "" {
		return nil, fmt.Errorf("LND_CERT_PATH cannot be empty")
	}
	macaroonPath := os.Getenv("LND_MACAROON_PATH")
	if macaroonPath == "" {
		return nil, fmt.Errorf("LND_MACAROON_PATH cannot be empty")
	}

	walletPath := os.Getenv("WALLET_PATH")
	if len(walletPath) == 0 {
		walletPath = "./.cashu-wallet"
	}

	mint := os.Getenv("MINT")
	if len(mint) == 0 {
		mint = "http://127.0.0.1:3338"
	}

	return &Env{
		grpcHost:     host,
		tlsCertPath:  certPath,
		macaroonPath: macaroonPath,
		walletPath:   walletPath,
		mint:         mint,
	}, nil
}
