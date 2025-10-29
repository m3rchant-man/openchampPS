#!/bin/bash

# This script *runs* the official postgresql:17 container
# using variables from the .env file at runtime.
# This is the recommended practice.

ENV_FILE=".env"
CONTAINER_NAME="openchampdb"
BASE_IMAGE="postgresql17:latest"

# --- 1. Check for .env file ---
if [ ! -f "$ENV_FILE" ]; then
    echo "❌ Error: $ENV_FILE not found."
    exit 1
fi

# --- 2. Load .env variables ---
echo "Loading variables from $ENV_FILE..."
export $(grep -v '^#' "$ENV_FILE" | xargs)

# --- 3. Check for required variables ---
if [ -z "$DB_USER" ] || [ -z "$DB_NAME" ] || [ -z "$DB_PASSWORD" ] || [ -z "$DB_PORT" ]; then
    echo "❌ Error: DB_USER, DB_NAME, DB_PASSWORD, or DB_PORT not set in $ENV_FILE."
    exit 1
fi

# --- 4. Rebuild Postgres image ---
echo "Rebuilding Postgres image..."
container build -t "$BASE_IMAGE" -f scripts/Dockerfile .

# --- 5. Stop and remove existing container (if any) ---
echo "Attempting to stop and remove old container named '$CONTAINER_NAME'..."
container stop "$CONTAINER_NAME" > /dev/null 2>&1
container rm "$CONTAINER_NAME" > /dev/null 2>&1

# --- 6. Run new container ---
echo "Starting new container '$CONTAINER_NAME'..."
container run -d \
    --name "$CONTAINER_NAME" \
    -e POSTGRES_USER="$DB_USER" \
    -e POSTGRES_DB="$DB_NAME" \
    -e POSTGRES_PASSWORD="$DB_PASSWORD" \
    --publish "127.0.0.1:$DB_PORT:5432/tcp" \
    -v "${CONTAINER_NAME}_data:~/openchamp_data" \
    "$BASE_IMAGE"

echo "✅ Container started successfully."
echo "-----------------------------------"
echo "  Host: localhost"
echo "  Port: $DB_PORT"
echo "  User: $DB_USER"
echo "  DB:   $DB_NAME"
echo "  Data Volume: ${CONTAINER_NAME}_data"
echo "-----------------------------------"