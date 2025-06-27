#!/bin/bash

set -euo pipefail

# Fire TV Rooms Deployment Script with Environment Support
# This script handles the complete deployment pipeline

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
ENVIRONMENT="${2:-production}"  # Default to production if not specified
STACK_NAME="fire-tv-rooms-${ENVIRONMENT}"
REGION="us-east-1"
ECR_REPOSITORY="fire-tv-rooms"

# Environment-specific configurations
case "$ENVIRONMENT" in
    "development"|"dev")
        DESIRED_COUNT=1
        MAX_CAPACITY=3
        REDIS_NODE_TYPE="cache.r6g.large"
        ;;
    "staging")
        DESIRED_COUNT=2
        MAX_CAPACITY=5
        REDIS_NODE_TYPE="cache.r6g.large"
        ;;
    "production"|"prod")
        DESIRED_COUNT=10
        MAX_CAPACITY=50
        REDIS_NODE_TYPE="cache.r6g.xlarge"
        ;;
    *)
        echo "Invalid environment: $ENVIRONMENT"
        echo "Valid environments: development, staging, production"
        exit 1
        ;;
esac

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    local missing_tools=()
    
    if ! command -v aws &> /dev/null; then
        missing_tools+=("aws-cli")
    fi
    
    if ! command -v docker &> /dev/null; then
        missing_tools+=("docker")
    fi
    
    if ! command -v jq &> /dev/null; then
        missing_tools+=("jq")
    fi
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        exit 1
    fi
    
    # Check AWS credentials
    if ! aws sts get-caller-identity &> /dev/null; then
        log_error "AWS credentials not configured"
        exit 1
    fi
    
    log_success "Prerequisites check passed"
}

# Get build information
get_build_info() {
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "unknown")
    BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    GIT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
    
    log_info "Build info: Version=$VERSION, Time=$BUILD_TIME, Commit=${GIT_COMMIT:0:8}, Environment=$ENVIRONMENT"
}

# Create ECR repository if it doesn't exist
# Create ECR repository if it doesn't exist
create_ecr_repository() {
    log_info "Checking ECR repository..."
    
    if ! aws ecr describe-repositories --repository-names "$ECR_REPOSITORY" --region "$REGION" &> /dev/null; then
        log_info "Creating ECR repository: $ECR_REPOSITORY"
        aws ecr create-repository \
            --repository-name "$ECR_REPOSITORY" \
            --region "$REGION" \
            --image-scanning-configuration scanOnPush=true \
            --encryption-configuration encryptionType=AES256
        
        # Set lifecycle policy with correct syntax
        aws ecr put-lifecycle-policy \
            --repository-name "$ECR_REPOSITORY" \
            --region "$REGION" \
            --lifecycle-policy-text '{
                "rules": [
                    {
                        "rulePriority": 1,
                        "description": "Keep last 10 tagged images",
                        "selection": {
                            "tagStatus": "tagged",
                            "tagPatternList": ["*"],
                            "countType": "imageCountMoreThan",
                            "countNumber": 10
                        },
                        "action": {
                            "type": "expire"
                        }
                    },
                    {
                        "rulePriority": 2,
                        "description": "Delete untagged images older than 1 day",
                        "selection": {
                            "tagStatus": "untagged",
                            "countType": "sinceImagePushed",
                            "countUnit": "days",
                            "countNumber": 1
                        },
                        "action": {
                            "type": "expire"
                        }
                    }
                ]
            }'
        
        log_success "ECR repository created"
    else
        log_info "ECR repository already exists"
    fi
}


# Build and push Docker image
build_and_push_image() {
    log_info "Building Docker image for environment: $ENVIRONMENT..."
    
    # Get ECR login token
    aws ecr get-login-password --region "$REGION" | \
        docker login --username AWS --password-stdin \
        "$(aws sts get-caller-identity --query Account --output text).dkr.ecr.$REGION.amazonaws.com"
    
    # Build image
    ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
    IMAGE_URI="$ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$ECR_REPOSITORY:$VERSION-$ENVIRONMENT"
    LATEST_URI="$ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$ECR_REPOSITORY:latest-$ENVIRONMENT"

    cd "$PROJECT_ROOT"
    
    docker build \
        --build-arg VERSION="$VERSION" \
        --build-arg BUILD_TIME="$BUILD_TIME" \
        --build-arg GIT_COMMIT="$GIT_COMMIT" \
        -t "$IMAGE_URI" \
        -t "$LATEST_URI" \
        "$PROJECT_ROOT"
    
    # Push images
    log_info "Pushing Docker images..."
    docker push "$IMAGE_URI"
    docker push "$LATEST_URI"
    
    log_success "Docker images pushed successfully"
    echo "IMAGE_URI=$IMAGE_URI"
}

# Deploy CloudFormation stack
deploy_cloudformation() {
    local image_uri="$1"
    
    log_info "Deploying CloudFormation stack for environment: $ENVIRONMENT..."
    
    # Check if stack exists
    if aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$REGION" &> /dev/null; then
        log_info "Updating existing stack..."
        OPERATION="update-stack"
    else
        log_info "Creating new stack..."
        OPERATION="create-stack"
    fi
    
    # Get VPC and subnet information with proper formatting
    VPC_ID=$(aws ec2 describe-vpcs --filters "Name=is-default,Values=true" --query "Vpcs[0].VpcId" --output text --region "$REGION")
    
    # Get public subnets as comma-separated string
    PUBLIC_SUBNETS=$(aws ec2 describe-subnets \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=map-public-ip-on-launch,Values=true" \
        --query "Subnets[].SubnetId" \
        --output text \
        --region "$REGION" | tr '\t' ',')
    
    # Get private subnets as comma-separated string
    PRIVATE_SUBNETS=$(aws ec2 describe-subnets \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=map-public-ip-on-launch,Values=false" \
        --query "Subnets[].SubnetId" \
        --output text \
        --region "$REGION" | tr '\t' ',')
    
    # If no private subnets, use public subnets
    if [ -z "$PRIVATE_SUBNETS" ] || [ "$PRIVATE_SUBNETS" = "" ]; then
        PRIVATE_SUBNETS="$PUBLIC_SUBNETS"
        log_warning "No private subnets found, using public subnets for ECS tasks"
    fi
    
    # Debug: Print subnet values
    log_info "VPC ID: $VPC_ID"
    log_info "Public Subnets: $PUBLIC_SUBNETS"
    log_info "Private Subnets: $PRIVATE_SUBNETS"
    
    # For staging/dev, skip certificate requirement (use HTTP only)
    if [ "$ENVIRONMENT" = "staging" ] || [ "$ENVIRONMENT" = "development" ]; then
        CERTIFICATE_ARN="NONE"
    log_warning "Deploying without HTTPS for $ENVIRONMENT environment"
    else
        CERTIFICATE_ARN="arn:aws:acm:$REGION:$(aws sts get-caller-identity --query Account --output text):certificate/your-certificate-id"
    fi
    
    # Ensure we're in the project root directory
    cd "$PROJECT_ROOT"
    
    # Deploy stack with correct parameters
    aws cloudformation "$OPERATION" \
        --stack-name "$STACK_NAME" \
        --template-body "file://infrastructure/aws/cloudformation/main.yaml" \
        --parameters \
            "ParameterKey=VpcId,ParameterValue=$VPC_ID" \
            "ParameterKey=PublicSubnetIds,ParameterValue=\"$PUBLIC_SUBNETS\"" \
            "ParameterKey=PrivateSubnetIds,ParameterValue=\"$PRIVATE_SUBNETS\"" \
            "ParameterKey=ECRImageURI,ParameterValue=$image_uri" \
            "ParameterKey=CertificateArn,ParameterValue=$CERTIFICATE_ARN" \
            "ParameterKey=Environment,ParameterValue=$ENVIRONMENT" \
            "ParameterKey=DesiredCount,ParameterValue=$DESIRED_COUNT" \
            "ParameterKey=MaxCapacity,ParameterValue=$MAX_CAPACITY" \
            "ParameterKey=RedisNodeType,ParameterValue=$REDIS_NODE_TYPE" \
        --capabilities CAPABILITY_NAMED_IAM \
        --region "$REGION"
    
    # Wait for stack operation to complete
    log_info "Waiting for stack operation to complete..."
    if [ "$OPERATION" = "create-stack" ]; then
        aws cloudformation wait stack-create-complete --stack-name "$STACK_NAME" --region "$REGION"
    else
        aws cloudformation wait stack-update-complete --stack-name "$STACK_NAME" --region "$REGION"
    fi
    
    log_success "CloudFormation stack deployed successfully"
}

# Get stack outputs
get_stack_outputs() {
    log_info "Getting stack outputs..."
    
    ALB_ENDPOINT=$(aws cloudformation describe-stacks \
        --stack-name "$STACK_NAME" \
        --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='ALBEndpoint'].OutputValue" \
        --output text)
    
    REDIS_ENDPOINT=$(aws cloudformation describe-stacks \
        --stack-name "$STACK_NAME" \
        --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='RedisPrimaryEndpoint'].OutputValue" \
        --output text)
    
    DYNAMODB_TABLE=$(aws cloudformation describe-stacks \
        --stack-name "$STACK_NAME" \
        --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='DynamoDBTableName'].OutputValue" \
        --output text)
    
    log_success "Deployment completed successfully for environment: $ENVIRONMENT!"
    echo ""
    echo "=== Deployment Information ==="
    echo "Environment: $ENVIRONMENT"
    echo "Stack Name: $STACK_NAME"
    echo "Load Balancer Endpoint: https://$ALB_ENDPOINT"
    echo "Redis Endpoint: $REDIS_ENDPOINT"
    echo "DynamoDB Table: $DYNAMODB_TABLE"
    echo "WebSocket URL: wss://$ALB_ENDPOINT/ws"
    echo "Health Check: https://$ALB_ENDPOINT/health"
    echo "API Base URL: https://$ALB_ENDPOINT/api/v1"
    echo "ECS Tasks: $DESIRED_COUNT (max: $MAX_CAPACITY)"
    echo "Redis Node: $REDIS_NODE_TYPE"
}

# Verify deployment
verify_deployment() {
    log_info "Verifying deployment..."
    
    # Wait for service to be stable
    ECS_CLUSTER=$(aws cloudformation describe-stacks \
        --stack-name "$STACK_NAME" \
        --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='ECSClusterName'].OutputValue" \
        --output text)
    
    ECS_SERVICE=$(aws cloudformation describe-stacks \
        --stack-name "$STACK_NAME" \
        --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='ECSServiceName'].OutputValue" \
        --output text)
    
    log_info "Waiting for ECS service to stabilize..."
    aws ecs wait services-stable \
        --cluster "$ECS_CLUSTER" \
        --services "$ECS_SERVICE" \
        --region "$REGION"
    
    # Test health endpoint
    ALB_ENDPOINT=$(aws cloudformation describe-stacks \
        --stack-name "$STACK_NAME" \
        --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='ALBEndpoint'].OutputValue" \
        --output text)
    
    log_info "Testing health endpoint..."
    if curl -f -s "https://$ALB_ENDPOINT/health" > /dev/null; then
        log_success "Health check passed"
    else
        log_warning "Health check failed - service may still be starting"
    fi
}

# Main deployment function
main() {
    log_info "Starting Fire TV Rooms deployment for environment: $ENVIRONMENT..."
    
    check_prerequisites
    get_build_info
    create_ecr_repository
    
    IMAGE_URI=$(build_and_push_image | grep "IMAGE_URI=" | cut -d'=' -f2)
    
    deploy_cloudformation "$IMAGE_URI"
    get_stack_outputs
    verify_deployment
    
    log_success "Deployment completed successfully for environment: $ENVIRONMENT!"
}

# Handle script arguments
case "${1:-deploy}" in
    "deploy")
        main
        ;;
    "destroy")
        log_warning "Destroying stack: $STACK_NAME (environment: $ENVIRONMENT)"
        read -p "Are you sure? (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            aws cloudformation delete-stack --stack-name "$STACK_NAME" --region "$REGION"
            log_info "Stack deletion initiated for environment: $ENVIRONMENT"
        fi
        ;;
    "status")
        aws cloudformation describe-stacks --stack-name "$STACK_NAME" --region "$REGION" --query "Stacks[0].StackStatus" --output text
        ;;
    *)
        echo "Usage: $0 [deploy|destroy|status] [environment]"
        echo "Environments: development, staging, production (default: production)"
        echo ""
        echo "Examples:"
        echo "  $0 deploy staging          # Deploy to staging environment"
        echo "  $0 deploy production       # Deploy to production environment" 
        echo "  $0 destroy staging         # Destroy staging environment"
        echo "  $0 status production       # Check production deployment status"
        exit 1
        ;;
esac
