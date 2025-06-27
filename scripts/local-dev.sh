#!/bin/bash
# scripts/test-local.sh

set -euo pipefail

echo "🧪 Starting local Fire TV Rooms testing..."

# Check AWS CLI
if ! command -v aws &>/dev/null; then
  echo "❌ AWS CLI not found. Please install it."
  exit 1
fi

# Set dummy AWS credentials for DynamoDB Local
export AWS_ACCESS_KEY_ID=dummyKey123
export AWS_SECRET_ACCESS_KEY=dummySecret123

# Start Redis, DynamoDB, Redis Commander only (not app yet)
docker-compose up -d redis redis-commander dynamodb-local

# Wait for DynamoDB to be ready
echo "⏳ Waiting for DynamoDB Local to be ready..."
for i in {1..10}; do
  if curl -sf http://localhost:8000 > /dev/null; then
    echo "✅ DynamoDB is up"
    break
  fi
  echo "⌛ Waiting... ($i)"
  sleep 2
done

# Create DynamoDB table if missing
echo "📁 Checking DynamoDB table..."
if aws dynamodb describe-table --table-name fire-tv-rooms \
    --endpoint-url http://localhost:8000 --region us-east-1 > /dev/null 2>&1; then
  echo "📂 Table already exists."
else
  echo "📁 Creating DynamoDB table..."
  aws dynamodb create-table \
      --table-name fire-tv-rooms \
      --attribute-definitions \
          AttributeName=id,AttributeType=S \
          AttributeName=gsi1pk,AttributeType=S \
          AttributeName=gsi1sk,AttributeType=S \
      --key-schema \
          AttributeName=id,KeyType=HASH \
      --global-secondary-indexes \
          '[{
              "IndexName": "GSI1",
              "KeySchema": [
                  {"AttributeName": "gsi1pk", "KeyType": "HASH"},
                  {"AttributeName": "gsi1sk", "KeyType": "RANGE"}
              ],
              "Projection": {"ProjectionType": "ALL"},
              "ProvisionedThroughput": {
                  "ReadCapacityUnits": 5,
                  "WriteCapacityUnits": 5
              }
          }]' \
      --billing-mode PAY_PER_REQUEST \
      --endpoint-url http://localhost:8000 \
      --region us-east-1
fi

# ✅ Start app container AFTER table exists
echo "🚀 Starting app container..."
docker-compose up -d app

# Health check
echo "🏥 Testing health endpoint..."
for i in {1..10}; do
  if curl -sf http://localhost:8080/health > /dev/null; then
    echo "✅ Health check passed"
    break
  fi
  echo "⌛ Waiting for health... ($i)"
  sleep 2
done

# WebSocket test info
if command -v wscat &>/dev/null; then
  echo "🔌 Run WebSocket test: wscat -c ws://localhost:8080/ws"
else
  echo "ℹ️ Install wscat (npm install -g wscat) to test WebSocket manually"
fi

# Final banner
echo "✅ Local testing environment ready!"
echo "📊 Redis Commander: http://localhost:8081"
echo "🗄️ DynamoDB Local: http://localhost:8000"
echo "🌐 Application: http://localhost:8080"
