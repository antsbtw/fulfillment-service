#!/bin/bash
set -e

echo "=== Deploying fulfillment-service ==="

docker exec v3-postgres psql -U saas_user -d postgres -tc "SELECT 1 FROM pg_database WHERE datname = 'fulfillment_db'" | grep -q 1 || \
  docker exec v3-postgres psql -U saas_user -d postgres -c "CREATE DATABASE fulfillment_db;"

docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d

echo "Waiting for service to be healthy..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:8014/health > /dev/null 2>&1; then
    echo "=== fulfillment-service deployed successfully ==="
    docker ps --filter name=fulfillment-service --format "table {{.Names}}\t{{.Status}}"
    exit 0
  fi
  sleep 2
done

echo "=== WARNING: health check not passing after 60s ==="
docker logs fulfillment-service --tail 30
exit 1
