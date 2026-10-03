#!/usr/bin/env bash
cd "$(dirname "${BASH_SOURCE[0]}")/cli"
SPRINKLES_API_KEY=d8ba04be-5ed6-4b65-9342-67bc0c327862 go run cmd/sprinkles/main.go submit --url http://localhost:8080 sample-job.json
