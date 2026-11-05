# Codex2API Deployment Guide

This document describes the ways to deploy Codex2API in detail.

## Table of contents

- [Deployment modes overview](#deployment-modes-overview)
- [Quick start](#quick-start)
- [Docker deployment](#docker-deployment)
- [Render image deployment](#render-image-deployment)
- [Local development](#local-development)
- [Production configuration](#production-configuration)
- [Upgrade guide](#upgrade-guide)
- [Backup and restore](#backup-and-restore)

---

## Deployment modes overview

| Mode | Use case | Database | Cache |
|------|----------|--------|------|
| **Gateway only** (`docker-compose.yml`) | Recommended: external PostgreSQL such as Neon, or a single machine | PostgreSQL via `DATABASE_URL`, or SQLite at `/data` | In-memory |
| **Full stack** (`docker-compose.full.yml`) | Self-contained host | Bundled PostgreSQL | Bundled Redis |
| **Local source** | Development and debugging | Optional | Optional |

---

## Quick start

### 1. Gateway only (recommended)

`docker-compose.yml` runs the gateway alone. Point it at an external PostgreSQL such as Neon with `DATABASE_URL`, or use SQLite on the `/data` volume.

```bash
# Clone the repository
git clone https://github.com/rbonweb/codex2api.git
cd codex2api

# Configure the environment
cp .env.example .env
# Edit the .env file and set the required parameters:
#   PostgreSQL such as Neon: DATABASE_URL=postgresql://USER:PASSWORD@HOST/DBNAME?sslmode=require
#   SQLite: DATABASE_DRIVER=sqlite and DATABASE_PATH=/data/codex2api.db

# Start the service
docker compose pull
docker compose up -d

# View logs
docker compose logs -f codex2api
```

### 2. Full stack with bundled PostgreSQL and Redis

```bash
git clone https://github.com/rbonweb/codex2api.git
cd codex2api
cp .env.example .env
docker compose -f docker-compose.full.yml pull
docker compose -f docker-compose.full.yml up -d
```

---

## Docker deployment

### Gateway only

**docker-compose.yml services:**

```yaml
services:
  codex2api:    # main application service (single container)
```

**Data persistence:**

| Volume | Purpose |
|------|------|
| codex2api_data | `/data` in the container: the SQLite database file and image assets |

**Deployment procedure:**

```bash
# 1. Prepare the environment file
cp .env.example .env

# 2. Edit the .env configuration
# - CODEX_PORT: service port
# - ADMIN_SECRET: admin panel password
# - PostgreSQL such as Neon: DATABASE_URL=postgresql://USER:PASSWORD@HOST/DBNAME?sslmode=require
#   (use the direct host, not the pooled host with -pooler)
# - SQLite instead: DATABASE_DRIVER=sqlite and DATABASE_PATH=/data/codex2api.db

# 3. Start the service
docker compose pull
docker compose up -d

# 4. Verify the status
docker compose ps
docker compose logs -f codex2api

# 5. Access the service
# Admin panel: http://localhost:8080/admin/
# API address: http://localhost:8080/v1/
```

### Full stack (PostgreSQL + Redis)

**docker-compose.full.yml services:**

```yaml
services:
  codex2api:    # main application service
  postgres:     # PostgreSQL database
  redis:        # Redis cache
```

**Data persistence:**

| Volume | Purpose |
|------|------|
| codex2api_pgdata | PostgreSQL data |
| codex2api_redisdata | Redis data |
| codex2api_image-assets | Image assets mounted at `/data` |

**Full deployment procedure:**

```bash
# 1. Prepare the environment file
cp .env.example .env

# 2. Edit the .env configuration
# - CODEX_PORT: service port
# - ADMIN_SECRET: admin panel password
# - DATABASE_*: database settings
# - REDIS_*: Redis settings

# 3. Start the service
docker compose -f docker-compose.full.yml pull
docker compose -f docker-compose.full.yml up -d

# 4. Verify the status
docker compose -f docker-compose.full.yml ps
docker compose -f docker-compose.full.yml logs -f codex2api
```

---

## Render image deployment

Render can run the image already built in GHCR directly, which suits a lightweight test environment or a small deployment.

### 1. Create an image-backed Web Service

In the Render Dashboard, create a `Web Service`, choose `Existing Image`, and enter this image address:

```text
ghcr.io/rbonweb/codex2api:latest
```

If the GHCR package is not public, first configure GitHub Container Registry credentials under Registry Credentials in Render.

**Fork releases (rbonweb/codex2api):** On every push to `main`, `.github/workflows/fork-release.yml` creates the next patch version tag (the larger of the fork's latest `vX.Y.Z` tag and the latest version in `CHANGELOG.md`, plus one) and, based on that tag, builds the public multi-architecture image `ghcr.io/rbonweb/codex2api` (tags `X.Y.Z`, `X.Y`, `vX.Y.Z`, `latest`) and a GitHub Release with binaries. Use `ghcr.io/rbonweb/codex2api:latest` as the Render image address to follow the latest version, or `ghcr.io/rbonweb/codex2api:X.Y.Z` to pin a version; changes that only touch `docs/` or Markdown do not trigger a release.

### 2. Configure environment variables

Free instances have no persistent disk, so for a test deployment we recommend SQLite plus in-memory cache, with the writable directories under `/tmp`:

```env
CODEX_PORT=10000
CODEX_BIND=0.0.0.0
DATABASE_DRIVER=sqlite
DATABASE_PATH=/tmp/codex2api.db
CACHE_DRIVER=memory
IMAGE_ASSET_DIR=/tmp/images
LOG_DIR=/tmp/logs
LOG_DISABLED=true
ADMIN_SECRET=replace-with-a-strong-secret
CODEX_ALLOW_ANONYMOUS=false
GIN_MODE=release
TZ=Asia/Shanghai
```

To keep data across redeploys, use an external PostgreSQL such as Neon instead: remove `DATABASE_DRIVER=sqlite` and `DATABASE_PATH`, and set `DATABASE_URL=postgresql://USER:PASSWORD@HOST/DBNAME?sslmode=require`. For Neon, use the direct host, not the pooled host with `-pooler`: the gateway uses session-level advisory locks, and `DATABASE_SCHEMA` also relies on setting `search_path` through startup parameters, and transaction-level connection pools support neither.

A Render Web Service exposes the service through `PORT=10000` by default; this project also reads `PORT`, but setting `CODEX_PORT=10000` explicitly is clearer.

### 3. Configure automatic deployment of the latest image

Render image-backed services do not redeploy automatically when the `latest` tag is updated. Use the Deploy Hook in the service Settings:

1. Open the Render service `Settings`.
2. Find `Deploy Hook` and copy the URL.
3. In the GitHub repository, go to `Settings` → `Secrets and variables` → `Actions`.
4. Add a repository secret:

```text
RENDER_DEPLOY_HOOK_URL=<Render Deploy Hook URL>
```

After that, once the `Build Docker Image` workflow has pushed the image successfully, its last step calls the hook automatically so Render pulls the image tag the service is configured with (such as `latest`) and deploys it; `.github/workflows/render-deploy.yml` is for manual redeploys.

You can also trigger the image build workflow manually; after the image is pushed successfully it moves on to the Render deploy workflow automatically:

```bash
gh workflow run docker-image.yml
```

To make Render pull the current `latest` image again without a new build, trigger the Render deploy workflow on its own:

```bash
gh workflow run render-deploy.yml
```

### 4. Verify

After the deployment completes, visit:

```text
https://<your-service>.onrender.com/admin/
https://<your-service>.onrender.com/health
```

Note: Render free instances suit testing and are not recommended for real tokens or production traffic; after the instance sleeps, restarts or is redeployed, the SQLite data and the image gallery in `/tmp` may be lost.

---

## Local development

### Requirements

- Go 1.26.10+
- Node.js 22.12+
- PostgreSQL 14+ (optional, SQLite can be used)
- Redis 7+ (optional, in-memory cache can be used)

### Backend development

```bash
# 1. Install dependencies
go mod download

# 2. Configure the environment
cp .env.example .env
# Edit .env to configure the local database

# 3. Build the frontend (required, because Go embeds it with go:embed)
cd frontend && npm ci && npm run build && cd ..

# 4. Start the backend
go run .
```

### Frontend development

```bash
cd frontend

# Install dependencies
npm ci

# Run validation after changing the frontend
npm test
npm run typecheck

# Start the development server
npm run dev
```

The Vite configuration already includes proxy rules; during development, visit `http://localhost:5173/admin/` and API requests are proxied to the backend automatically.

**vite.config.js proxy configuration:**

```javascript
server: {
  proxy: {
    '/api': 'http://localhost:8080',
    '/health': 'http://localhost:8080',
    '/v1': 'http://localhost:8080',
  }
}
```

---

## Production configuration

### 1. Environment variables

**Required settings:**

```bash
# Service port
CODEX_PORT=8080

# Admin panel password (a strong password is recommended)
ADMIN_SECRET=your-strong-password-here

# Database settings (PostgreSQL mode)
DATABASE_DRIVER=postgres
DATABASE_HOST=postgres
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=your-db-password
DATABASE_NAME=codex2api

# Redis settings
CACHE_DRIVER=redis
REDIS_ADDR=redis:6379
REDIS_USERNAME=
REDIS_PASSWORD=your-redis-password
REDIS_DB=0
REDIS_TLS=false
REDIS_INSECURE_SKIP_VERIFY=false

# Time zone
TZ=Asia/Shanghai
```

Cloud Redis (such as Aiven or Upstash) usually requires TLS; you can use the `rediss://` connection string provided by the platform directly:

```env
CACHE_DRIVER=redis
REDIS_ADDR=rediss://default:your-redis-password@your-redis-host:6379/0
```

**Optional settings:**

```bash
# Fast scheduler
FAST_SCHEDULER_ENABLED=true

# Experimental: plaintext compatibility for Codex Multi-Agent v2 third-party sub-agents (off by default)
# CODEX_MULTI_AGENT_V2_COMPAT=false
# For details and the gradual rollout steps, see docs/CODEX_MULTI_AGENT_V2_COMPAT.md
```

### 2. System settings (through the admin panel)

After the first start, visit `/admin/settings` to configure:

| Setting | Recommended value | Description |
|------|--------|------|
| Max Concurrency | 2-4 | Maximum concurrency per account |
| Global RPM | 0 or 1000+ | 0 means no rate limit |
| Test Model | gpt-5.4 | Model used for test connection |
| Test Concurrency | 50 | Batch test concurrency |
| PgMax Conns | 50 | PostgreSQL connection pool |
| Redis Pool Size | 30 | Redis connection pool |

### 3. Reverse proxy

**Nginx configuration example:**

```nginx
server {
    listen 80;
    server_name codex.example.com;

    # Force HTTPS (recommended for production)
    return 301 https://$server_name$request_uri;
}

server {
    listen 443 ssl http2;
    server_name codex.example.com;

    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    # Admin panel (extra authentication can be added)
    location /admin/ {
        proxy_pass http://localhost:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    # API endpoints
    location /v1/ {
        proxy_pass http://localhost:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;

        # Streaming response optimization
        proxy_buffering off;
        proxy_cache off;
    }

    # Health check
    location /health {
        proxy_pass http://localhost:8080;
    }
}
```

### 4. Docker Compose production configuration

```yaml
version: '3.8'

services:
  codex2api:
    image: ghcr.io/rbonweb/codex2api:latest
    container_name: codex2api
    restart: unless-stopped
    env_file:
      - .env
    ports:
      - "127.0.0.1:8080:8080"  # listen on localhost only, expose through nginx
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - codex2api
    logging:
      driver: "json-file"
      options:
        max-size: "100m"
        max-file: "3"

  postgres:
    image: postgres:15-alpine
    container_name: codex2api-postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: ${DATABASE_USER}
      POSTGRES_PASSWORD: ${DATABASE_PASSWORD}
      POSTGRES_DB: ${DATABASE_NAME}
    volumes:
      - pgdata:/var/lib/postgresql/data
    networks:
      - codex2api
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${DATABASE_USER} -d ${DATABASE_NAME}"]
      interval: 5s
      timeout: 5s
      retries: 5

  redis:
    image: redis:7-alpine
    container_name: codex2api-redis
    restart: unless-stopped
    command: redis-server --requirepass ${REDIS_PASSWORD}
    volumes:
      - redisdata:/data
    networks:
      - codex2api
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 5s
      retries: 5

volumes:
  pgdata:
  redisdata:

networks:
  codex2api:
    driver: bridge
```

---

## Upgrade guide

### Standard upgrade procedure

```bash
# 1. Back up the database (important!)
docker exec codex2api-postgres pg_dump -U codex2api codex2api > backup_$(date +%Y%m%d_%H%M%S).sql

# 2. Pull the new version
git pull
docker compose -f docker-compose.full.yml pull

# 3. Rolling update (zero downtime)
docker compose -f docker-compose.full.yml up -d

# 4. Verify the status
docker compose -f docker-compose.full.yml ps
docker compose -f docker-compose.full.yml logs -f codex2api

# 5. Health check
curl http://localhost:8080/health
```

### Downgrading

```bash
# 1. Stop the service
docker compose -f docker-compose.full.yml down

# 2. Restore the database
docker exec -i codex2api-postgres psql -U codex2api codex2api < backup_xxx.sql

# 3. Start with the old version
# Edit docker-compose.full.yml and set image:tag (for example ghcr.io/rbonweb/codex2api:X.Y.Z)
docker compose -f docker-compose.full.yml up -d
```

### Gateway-only upgrade

```bash
# Back up the SQLite database (skip when using an external PostgreSQL such as Neon)
cp /path/to/codex2api.db /path/to/codex2api.db.backup_$(date +%Y%m%d_%H%M%S)

# Upgrade
docker compose pull
docker compose up -d
```

---

## Backup and restore

### PostgreSQL backup

**Automatic backup script:**

```bash
#!/bin/bash
# backup.sh

BACKUP_DIR="/backup/codex2api"
DATE=$(date +%Y%m%d_%H%M%S)
CONTAINER="codex2api-postgres"
DB_NAME="codex2api"
DB_USER="codex2api"

# Create the backup directory
mkdir -p $BACKUP_DIR

# Run the backup
docker exec $CONTAINER pg_dump -U $DB_USER $DB_NAME > $BACKUP_DIR/codex2api_$DATE.sql

# Keep the last 30 days of backups
find $BACKUP_DIR -name "*.sql" -mtime +30 -delete

echo "Backup completed: $BACKUP_DIR/codex2api_$DATE.sql"
```

**Add to a scheduled job:**

```bash
# Run the backup every day at 2 AM
0 2 * * * /path/to/backup.sh >> /var/log/codex2api-backup.log 2>&1
```

### PostgreSQL restore

```bash
# 1. Stop the application
docker compose -f docker-compose.full.yml stop codex2api

# 2. Restore the database
docker exec -i codex2api-postgres psql -U codex2api -d codex2api < backup_xxx.sql

# 3. Restart the service
docker compose -f docker-compose.full.yml start codex2api
```

### SQLite backup

```bash
# Back up
sqlite3 /data/codex2api.db ".backup '/backup/codex2api_$(date +%Y%m%d_%H%M%S).db'"

# Or simply copy
cp /data/codex2api.db /backup/codex2api_$(date +%Y%m%d_%H%M%S).db
```

### SQLite restore

```bash
# Stop the service
docker compose stop

# Restore the data
cp /backup/codex2api_xxx.db /data/codex2api.db

# Start the service
docker compose start
```

---

## Container and volume names

| Deployment mode | Container name | Data volumes |
|----------|--------|--------|
| Gateway only (`docker-compose.yml`) | codex2api | codex2api_data |
| Full stack (`docker-compose.full.yml`) | codex2api, codex2api-postgres, codex2api-redis | codex2api_pgdata, codex2api_redisdata, codex2api_image-assets |

**Note:** Data volumes of different modes are isolated from each other, so seeing empty data after switching compose files is normal.

### Index upgrade for large usage tables

The PostgreSQL gateway/upstream request ID indexes and the account generation index are built in the background after startup using `CREATE INDEX CONCURRENTLY`, which avoids scanning large tables and blocking writes inside the startup migration transaction.
Multiple instances coordinate the build through a database advisory lock; INVALID indexes left behind by an interrupted build are cleaned up online and rebuilt.
A failed build only affects the performance of the related queries; the service keeps running and retries on the next start, and the log shows the build result.
