# API 서비스명

> REST API 서비스 설명

## Base URL

| 환경 | URL |
|------|-----|
| 개발 | `http://localhost:3000` |
| 스테이징 | `https://api-staging.example.com` |
| 프로덕션 | `https://api.example.com` |

## 인증

모든 API 요청에는 `Authorization` 헤더가 필요합니다.

```
Authorization: Bearer <access_token>
```

토큰 발급:
```bash
curl -X POST https://api.example.com/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email": "user@example.com", "password": "password"}'
```

## Rate Limiting

| Plan | 요청 한도 | 윈도우 |
|------|----------|--------|
| Free | 100 req | 1시간 |
| Pro | 1,000 req | 1시간 |
| Enterprise | 무제한 | - |

초과 시 `429 Too Many Requests` 응답을 반환합니다.

## 공통 응답 형식

### 성공
```json
{
  "data": { ... },
  "meta": {
    "page": 1,
    "limit": 20,
    "total": 150
  }
}
```

### 에러
```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "리소스를 찾을 수 없습니다",
    "details": []
  }
}
```

## 에러 코드

| HTTP 상태 | 코드 | 설명 |
|-----------|------|------|
| 400 | `BAD_REQUEST` | 요청 형식 오류 |
| 401 | `UNAUTHORIZED` | 인증 실패 |
| 403 | `FORBIDDEN` | 권한 없음 |
| 404 | `NOT_FOUND` | 리소스 없음 |
| 409 | `CONFLICT` | 리소스 충돌 |
| 429 | `RATE_LIMITED` | 요청 한도 초과 |
| 500 | `INTERNAL_ERROR` | 서버 내부 오류 |

## 엔드포인트

### 상품

| Method | Path | 설명 | 인증 |
|--------|------|------|------|
| GET | `/api/products` | 상품 목록 | 불필요 |
| GET | `/api/products/:id` | 상품 상세 | 불필요 |
| POST | `/api/products` | 상품 등록 | 필요 (관리자) |
| PUT | `/api/products/:id` | 상품 수정 | 필요 (관리자) |
| DELETE | `/api/products/:id` | 상품 삭제 | 필요 (관리자) |

### 상세 문서
- [Swagger UI](/api-docs) — 인터랙티브 API 테스트
- [ReDoc](/redoc) — 읽기 좋은 API 문서

## SDK / 클라이언트

```bash
# JavaScript
npm install @example/api-client

# Python
pip install example-api-client
```

```javascript
import { ApiClient } from '@example/api-client';

const client = new ApiClient({ token: 'your-token' });
const products = await client.products.list({ category: '전자제품' });
```

## Changelog

### v1.1.0 (2024-03-01)
- 상품 검색 API 추가
- 페이지네이션 커서 방식 지원

### v1.0.0 (2024-01-15)
- 최초 릴리즈
- 상품 CRUD, 주문, 인증 API
