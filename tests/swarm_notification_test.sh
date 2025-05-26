#!/bin/bash

# Test script for Docker Swarm container notifications
# This script sets up a small Docker Swarm environment and tests the notification functionality

set -e

echo "Setting up Docker Swarm test environment..."

# Check if Docker is running
if ! docker info > /dev/null 2>&1; then
    echo "Error: Docker is not running or not accessible"
    exit 1
fi

# Initialize a Swarm if not already in Swarm mode
if ! docker info | grep -q "Swarm: active"; then
    echo "Initializing Docker Swarm..."
    docker swarm init
else
    echo "Docker Swarm is already active"
fi

# Create a simple service that we'll notify
echo "Creating test service with notification label..."
docker service create --name test-notification \
    --replicas 2 \
    --label notify=true \
    alpine:latest \
    /bin/sh -c "trap 'echo \"Received SIGHUP\"' HUP; while true; do sleep 1; done"

echo "Waiting for service to start..."
sleep 5

# Check that the service is running
if ! docker service ls | grep -q "test-notification"; then
    echo "Error: Test service failed to start"
    exit 1
fi

# Create a simple template for testing
echo "Creating test template..."
cat > /tmp/test-template.tmpl << 'EOF'
# Test template for Swarm notifications
{{ range $container := . }}
Container: {{ $container.Name }}
{{ end }}
EOF

echo "Building docker-gen..."
go build -o ./bin/docker-gen ./cmd/docker-gen

echo "Running docker-gen with Swarm notification..."
# Run docker-gen with verbose logging
./bin/docker-gen -notify-filter "label=notify=true" -notify-signal 1 -swarm-endpoint unix:///var/run/docker.sock /tmp/test-template.tmpl /tmp/output.conf

# Wait for logs to appear (in a real test you'd examine the logs)
sleep 3

echo "Checking task logs for notification..."
TASKS=$(docker service ps --format '{{.ID}}' test-notification)
for TASK_ID in $TASKS; do
    CONTAINER_ID=$(docker inspect --format '{{.Status.ContainerStatus.ContainerID}}' $TASK_ID)
    if [ ! -z "$CONTAINER_ID" ]; then
        echo "Checking logs for container $CONTAINER_ID"
        docker logs $CONTAINER_ID | grep -q "Received SIGHUP" && echo "SUCCESS: Notification received!" || echo "FAIL: No notification received"
    fi
done

echo "Cleaning up..."
docker service rm test-notification
rm /tmp/test-template.tmpl /tmp/output.conf

echo "Test completed"
