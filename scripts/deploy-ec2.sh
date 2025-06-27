#!/bin/bash
set -euo pipefail

EC2_HOST="15.206.89.216"
KEY_PATH="/c/Users/sahil/Downloads/Sahil_PC.pem"


echo "🚀 Deploying Fire TV Rooms to EC2..."

# Build Docker image locally
docker build -t fire-tv-rooms .

# Save image to tar file
docker save fire-tv-rooms > fire-tv-rooms.tar

# Copy to EC2
scp -i $KEY_PATH fire-tv-rooms.tar ubuntu@$EC2_HOST:/home/ubuntu/

# Deploy on EC2
ssh -i $KEY_PATH ubuntu@$EC2_HOST << 'EOF'
  # Install Docker if not installed
  sudo yum update -y
  sudo yum install -y docker
  sudo systemctl start docker
  sudo usermod -a -G docker ec2-user

  # Load and run the image
  docker load < fire-tv-rooms.tar
  
  # Stop existing container if running
  docker stop fire-tv-rooms || true
  docker rm fire-tv-rooms || true
  
  # Run new container
  docker run -d \
    --name fire-tv-rooms \
    --restart unless-stopped \
    -p 8080:8080 \
    -e ENVIRONMENT=production \
    -e REDIS_ADDR=localhost:6379 \
    fire-tv-rooms

  echo "✅ Deployment complete!"
EOF

# Cleanup
rm fire-tv-rooms.tar

echo "🌐 App running at: http://$EC2_HOST:8080"
