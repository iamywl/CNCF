# 환경 변수 설정 가이드

> 마지막 업데이트: 2024-03-01

## 개요

이 문서는 E-Commerce API 서버에서 사용하는 **모든 환경 변수**를 설명합니다.
각 변수의 용도, 기본값, 민감도를 명시하여 설정 실수를 방지합니다.

## 환경 변수 전체 목록

### 서버 설정

| 변수명 | 설명 | 기본값 | 필수 | 민감 |
|--------|------|--------|------|------|
| `NODE_ENV` | 실행 환경 | `development` | O | - |
| `PORT` | 서버 포트 | `3000` | - | - |
| `HOST` | 바인딩 호스트 | `0.0.0.0` | - | - |
| `LOG_LEVEL` | 로그 레벨 (debug/info/warn/error) | `info` | - | - |

### 데이터베이스

| 변수명 | 설명 | 기본값 | 필수 | 민감 |
|--------|------|--------|------|------|
| `DB_HOST` | PostgreSQL 호스트 | `localhost` | O | - |
| `DB_PORT` | PostgreSQL 포트 | `5432` | - | - |
| `DB_NAME` | 데이터베이스명 | `ecommerce` | O | - |
| `DB_USER` | DB 사용자명 | - | O | O |
| `DB_PASSWORD` | DB 비밀번호 | - | O | **비밀** |
| `DB_SSL` | SSL 연결 여부 | `false` | - | - |
| `DB_POOL_SIZE` | 커넥션 풀 크기 | `10` | - | - |

### Redis

| 변수명 | 설명 | 기본값 | 필수 | 민감 |
|--------|------|--------|------|------|
| `REDIS_URL` | Redis 접속 URL | `redis://localhost:6379` | O | - |
| `REDIS_PASSWORD` | Redis 비밀번호 | - | - | **비밀** |

### JWT 인증

| 변수명 | 설명 | 기본값 | 필수 | 민감 |
|--------|------|--------|------|------|
| `JWT_SECRET` | JWT 서명 키 | - | O | **비밀** |
| `JWT_EXPIRES_IN` | Access Token 유효기간 | `1h` | - | - |
| `JWT_REFRESH_EXPIRES_IN` | Refresh Token 유효기간 | `7d` | - | - |

### 외부 서비스

| 변수명 | 설명 | 기본값 | 필수 | 민감 |
|--------|------|--------|------|------|
| `PAYMENT_API_URL` | PG사 API URL | - | O | - |
| `PAYMENT_API_KEY` | PG사 API 키 | - | O | **비밀** |
| `SENDGRID_API_KEY` | SendGrid 이메일 API 키 | - | O | **비밀** |
| `S3_BUCKET` | AWS S3 버킷명 | - | O | - |
| `S3_ACCESS_KEY` | AWS Access Key | - | O | **비밀** |
| `S3_SECRET_KEY` | AWS Secret Key | - | O | **비밀** |
| `S3_REGION` | AWS 리전 | `ap-northeast-2` | - | - |

### 모니터링

| 변수명 | 설명 | 기본값 | 필수 | 민감 |
|--------|------|--------|------|------|
| `SENTRY_DSN` | Sentry 에러 트래킹 DSN | - | - | - |
| `SLACK_WEBHOOK_URL` | Slack 알림 웹훅 | - | - | - |

---

## 환경별 설정 예시

### 개발 환경 (.env.development)

```bash
NODE_ENV=development
PORT=3000
LOG_LEVEL=debug

DB_HOST=localhost
DB_PORT=5432
DB_NAME=ecommerce_dev
DB_USER=postgres
DB_PASSWORD=localpassword
DB_POOL_SIZE=5

REDIS_URL=redis://localhost:6379

JWT_SECRET=dev-secret-key-not-for-production
JWT_EXPIRES_IN=24h

# 개발 환경에서는 외부 서비스 모킹
PAYMENT_API_URL=http://localhost:9090/mock-payment
```

### 스테이징 환경

```bash
NODE_ENV=staging
LOG_LEVEL=info

DB_HOST=staging-db.internal
DB_NAME=ecommerce_staging
DB_SSL=true
DB_POOL_SIZE=10

# 비밀 변수는 K8s Secret 또는 AWS Secrets Manager에서 주입
# DB_USER, DB_PASSWORD, JWT_SECRET 등은 절대 파일에 기록하지 않음
```

### 프로덕션 환경

```bash
NODE_ENV=production
LOG_LEVEL=warn

DB_HOST=prod-db.internal
DB_NAME=ecommerce
DB_SSL=true
DB_POOL_SIZE=20

# 모든 비밀 변수는 Kubernetes Secret으로 관리
# kubectl create secret generic ecommerce-secrets \
#   --from-literal=DB_USER=prod_user \
#   --from-literal=DB_PASSWORD=... \
#   --from-literal=JWT_SECRET=...
```

---

## 비밀 변수 관리 원칙

1. **절대 Git에 커밋하지 않음** — `.gitignore`에 `.env` 추가
2. **Kubernetes Secret 사용** — 프로덕션은 K8s Secret으로 주입
3. **로테이션 주기** — JWT_SECRET, API 키는 분기별 로테이션
4. **최소 권한** — DB 사용자는 필요한 권한만 부여

---

## 새 환경 변수 추가 시 체크리스트

- [ ] 이 문서에 변수 추가 (용도, 기본값, 민감도)
- [ ] `.env.example` 파일에 추가 (값은 비우거나 예시)
- [ ] 모든 환경(dev/staging/prod)에 값 설정
- [ ] 비밀 변수인 경우 K8s Secret에 추가
- [ ] 팀 Slack에 공유
