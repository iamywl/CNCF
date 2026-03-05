# PoC-11: gRPC 에러 처리 시뮬레이션

## 개념

gRPC는 HTTP 상태 코드 대신 독자적인 17개 상태 코드 체계를 사용한다. `Status` 구조체에 코드, 메시지, 상세 정보(details)를 담아 전달하며, HTTP/2 트레일러로 전송된다.

### 17개 상태 코드

| 코드 | 이름 | HTTP | 재시도 | 설명 |
|------|------|------|--------|------|
| 0 | OK | 200 | - | 성공 |
| 1 | CANCELLED | 499 | N | 클라이언트 취소 |
| 2 | UNKNOWN | 500 | N | 알 수 없는 에러 |
| 3 | INVALID_ARGUMENT | 400 | N | 잘못된 인자 |
| 4 | DEADLINE_EXCEEDED | 504 | Y | 기한 초과 |
| 5 | NOT_FOUND | 404 | N | 리소스 없음 |
| 6 | ALREADY_EXISTS | 409 | N | 이미 존재 |
| 7 | PERMISSION_DENIED | 403 | N | 권한 거부 |
| 8 | RESOURCE_EXHAUSTED | 429 | Y | 리소스 소진 |
| 9 | FAILED_PRECONDITION | 400 | N | 전제조건 실패 |
| 10 | ABORTED | 409 | Y | 트랜잭션 충돌 |
| 11 | OUT_OF_RANGE | 400 | N | 범위 초과 |
| 12 | UNIMPLEMENTED | 501 | N | 미구현 |
| 13 | INTERNAL | 500 | N | 내부 오류 |
| 14 | UNAVAILABLE | 503 | Y | 일시 불가 |
| 15 | DATA_LOSS | 500 | N | 데이터 유실 |
| 16 | UNAUTHENTICATED | 401 | N | 인증 실패 |

### Error ↔ Status 변환

```
Status.Err()    → error (OK이면 nil)
FromError(err)  → *Status (StatusError → 코드 보존, 일반 error → Unknown)
```

### WithDetails 패턴

```go
status.Newf(codes.InvalidArgument, "잘못된 요청").
    WithDetails(
        &errdetails.BadRequest{...},    // 필드별 오류 상세
        &errdetails.DebugInfo{...},     // 디버그 정보
    )
```

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
gRPC Status Codes 시뮬레이션
========================================

[1] gRPC 상태 코드 전체 목록
─────────────────────────────
   0 OK                     HTTP=200 재시도=N
   1 CANCELLED              HTTP=499 재시도=N
  ...
  14 UNAVAILABLE            HTTP=503 재시도=Y
  ...

[4] 서비스 호출 시뮬레이션
──────────────────────────
  ── 정상 요청 (userID="user-1") ──
  상태: OK
  결과: {"id": "user-1", "name": "홍길동"}

  ── 차단된 사용자 (userID="banned-user") ──
  상태: PERMISSION_DENIED (HTTP 403)
  메시지: 차단된 사용자입니다
  상세 정보:
    - [type.googleapis.com/google.rpc.ErrorInfo] reason: USER_BANNED
    - [type.googleapis.com/google.rpc.Help] 관리자에게 문의하세요
  → 동작: 접근 거부 페이지 표시
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `codes/codes.go` | 17개 상태 코드 정의 |
| `internal/status/status.go` | Status 구조체, WithDetails |
| `status/status.go` | 공개 API (New, Newf, FromError) |
