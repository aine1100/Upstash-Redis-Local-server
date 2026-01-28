#!/bin/bash

# Function to check if Docker is running
is_docker_running() {
    docker info > /dev/null 2>&1
    return $?
}

echo "🔍 Checking if Docker is running..."

if is_docker_running; then
    echo "✅ Docker is already running."
else
    echo "❌ Docker is not running. Attempting to start it..."

    # Check for Windows
    if [[ "$OSTYPE" == "msys" || "$OSTYPE" == "cygwin" ]]; then
        echo "🪟 Windows detected. Starting Docker Desktop..."
        # Try to start Docker Desktop using start command
        powershell.exe -Command "Start-Process 'C:\Program Files\Docker\Docker\Docker Desktop.exe'"
    elif [[ "$OSTYPE" == "darwin"* ]]; then
        echo "🍎 macOS detected. Starting Docker..."
        open -a Docker
    else
        echo "🐧 Linux detected. Please start the docker service (e.g., sudo systemctl start docker)"
        # On Linux we usually don't want to blindly sudo start services, but we can try
        # sudo systemctl start docker
    fi

    echo "⏳ Waiting for Docker to start..."
    
    # Wait for Docker to be ready (up to 2 minutes)
    counter=0
    max_retries=24
    until is_docker_running || [ $counter -eq $max_retries ]; do
        sleep 5
        counter=$((counter + 1))
        echo "Still waiting... ($((counter * 5))s)"
    done

    if is_docker_running; then
        echo "✅ Docker is now running."
    else
        echo "🚨 Failed to start Docker. Please start it manually and try again."
        exit 1
    fi
fi

echo "🚀 Starting containers with docker-compose (rebuilding if necessary)..."
docker-compose up -d --build

echo "✨ All set! Your Upstash Local server is starting."
echo "🔗 REST API: http://localhost:8000"
echo "🔗 Redis: localhost:6379"
