# 14. gRPC-Go 상태 코드 & 에러 처리 심화

## 개요

gRPC는 17개의 표준 상태 코드를 정의한다. 이 코드는 모든 gRPC 언어 구현에서 동일하며,
HTTP 상태 코드와는 별개의 체계이다. gRPC-Go에서 상태 코드는 `codes.Code` 타입으로,
에러는 `status.Status` 타입으로 표현된다.

**소스코드**:
- `codes/codes.go` — 17개 코드 상수
- `status/status.go` — Status 타입 (공개 API)
- `internal/status/status.go` — Status 내부 구현

---

## 1. 17개 gRPC 상태 코드

```go
// codes/codes.go:35
type Code uint32
```

### 전체 코드 일람

| 코드 | 값 | 이름 | 설명 | 생성 주체 |
|------|---|------|------|----------|
| 0 | OK | 성공 | 정상 완료 | 프레임워크/사용자 |
| 1 | Canceled | 취소됨 | 호출자가 취소 | 프레임워크 |
| 2 | Unknown | 알 수 없음 | 상태를 알 수 없는 에러 | 프레임워크 |
| 3 | InvalidArgument | 잘못된 인자 | 클라이언트 잘못된 요청 | 사용자 |
| 4 | DeadlineExceeded | 데드라인 초과 | 시간 초과 | 프레임워크 |
| 5 | NotFound | 찾을 수 없음 | 리소스 미존재 | 사용자 |
| 6 | AlreadyExists | 이미 존재 | 생성 시 중복 | 사용자 |
| 7 | PermissionDenied | 권한 거부 | 인가 실패 | 사용자/미들웨어 |
| 8 | ResourceExhausted | 리소스 소진 | 쿼터 초과, OOM | 프레임워크/사용자 |
| 9 | FailedPrecondition | 전제조건 실패 | 시스템 상태 부적합 | 사용자 |
| 10 | Aborted | 중단됨 | 동시성 문제 (트랜잭션) | 사용자 |
| 11 | OutOfRange | 범위 초과 | 유효 범위 밖 접근 | 사용자 |
| 12 | Unimplemented | 미구현 | 메서드 미구현 | 프레임워크/사용자 |
| 13 | Internal | 내부 오류 | 내부 불변성 위반 | 프레임워크 |
| 14 | Unavailable | 사용 불가 | 일시적 서비스 불가 | 프레임워크 |
| 15 | DataLoss | 데이터 손실 | 복구 불가 데이터 손실 | 사용자 |
| 16 | Unauthenticated | 미인증 | 인증 실패 | 프레임워크/미들웨어 |

### 코드 카테고리

```
클라이언트 에러 (재시도 의미 없음):
├── InvalidArgument (3)   — 요청 자체가 잘못됨
├── NotFound (5)          — 리소스 없음
├── AlreadyExists (6)     — 중복
├── PermissionDenied (7)  — 인가 실패
├── FailedPrecondition (9) — 상태 부적합
├── OutOfRange (11)       — 범위 초과
└── Unauthenticated (16)  — 인증 필요

서버/네트워크 에러 (재시도 가능):
├── Unavailable (14)      — 일시적 불가 → 재시도 권장
├── Aborted (10)          — 트랜잭션 충돌 → 상위 재시도
└── ResourceExhausted (8) — 쿼터 초과 → 대기 후 재시도

프레임워크 에러:
├── Canceled (1)          — 클라이언트가 취소
├── DeadlineExceeded (4)  — 타임아웃
├── Unknown (2)           — 알 수 없는 에러
├── Internal (13)         — 내부 버그
├── Unimplemented (12)    — 메서드 없음
└── DataLoss (15)         — 데이터 손실
```

---

## 2. 혼동하기 쉬운 코드 구분

### FailedPrecondition vs Aborted vs Unavailable

```
리트머스 테스트 (codes/codes.go:112-116에서 인용):

(a) Unavailable → 해당 호출만 재시도하면 됨
    예: 서버 일시 과부하, 네트워크 일시 단절

(b) Aborted → 상위 레벨에서 재시도
    예: read-modify-write 시퀀스 전체 재시도

(c) FailedPrecondition → 시스템 상태 수정 후 재시도
    예: 비어있지 않은 디렉토리 삭제 시도
```

### InvalidArgument vs FailedPrecondition

```
InvalidArgument:
  "시스템 상태와 무관하게 잘못된 인자"
  예: 잘못된 이메일 형식, 음수 수량

FailedPrecondition:
  "인자는 유효하지만 시스템 상태가 부적합"
  예: 잔액 부족으로 출금 실패
```

### PermissionDenied vs Unauthenticated

```
Unauthenticated (16):
  "누구인지 모름" — 인증 자격증명 없음/유효하지 않음
  → 로그인 필요

PermissionDenied (7):
  "누구인지는 알지만 권한이 없음"
  → 관리자에게 권한 요청 필요
```

### NotFound vs PermissionDenied (보안)

```
보안적 이유로 NotFound를 PermissionDenied 대신 사용하기도 함:
  → 리소스 존재 여부를 노출하지 않기 위해
  → 예: 다른 사용자의 주문 조회 시 "찾을 수 없음" 반환
```

---

## 3. Status 타입

### 공개 API (`status/status.go`)

```go
// status/status.go:45
type Status = status.Status   // internal/status.Status의 앨리어스
```

### 내부 구현 (`internal/status/status.go`)

```go
type Status struct {
    s *spb.Status   // google.rpc.Status protobuf
}

// google.rpc.Status protobuf:
// message Status {
//     int32 code = 1;
//     string message = 2;
//     repeated google.protobuf.Any details = 3;
// }
```

**왜 protobuf로 구현하는가?**

Status는 HTTP/2 트레일러를 통해 클라이언트에 전송된다. 상태 코드와 메시지는
`grpc-status`/`grpc-message` 헤더로 전송되지만, 상세 정보(details)는
protobuf로 직렬화하여 `grpc-status-details-bin` 바이너리 헤더로 전송된다.
이를 통해 구조화된 에러 정보를 언어 간에 교환할 수 있다.

### Status 생성

```go
// 기본 생성
st := status.New(codes.NotFound, "user not found")

// 포맷 문자열
st := status.Newf(codes.InvalidArgument, "field %s is invalid", fieldName)

// 에러로 직접 생성
err := status.Error(codes.PermissionDenied, "access denied")
err := status.Errorf(codes.Internal, "unexpected: %v", cause)
```

### Status → Error 변환

```go
// Status → error
st := status.New(codes.NotFound, "user not found")
err := st.Err()   // nil이 아닌 error 반환 (OK가 아닌 경우)

// OK 코드는 nil 반환
st := status.New(codes.OK, "")
err := st.Err()   // nil
```

### Error → Status 추출

```go
// error → Status (status/status.go:78)
func FromError(err error) (s *Status, ok bool) {
    // 1. nil → OK
    if err == nil {
        return nil, true
    }
    // 2. GRPCStatus() 메서드가 있으면 사용
    // 3. errors.As로 wrapped error 검색
    // 4. context.Canceled → Canceled
    // 5. context.DeadlineExceeded → DeadlineExceeded
    // 6. 기타 → Unknown
}

// 사용 예
st, ok := status.FromError(err)
if ok {
    code := st.Code()
    msg := st.Message()
}

// 또는 Convert (항상 Status 반환)
st := status.Convert(err)
code := st.Code()    // Unknown if not a gRPC error
```

### 코드 직접 추출

```go
// status/status.go
func Code(err error) codes.Code {
    return Convert(err).Code()
}

// 사용
if status.Code(err) == codes.NotFound {
    // 리소스를 찾을 수 없음
}
```

---

## 4. Error Details — 구조화된 에러 정보

gRPC의 강력한 기능 중 하나는 **에러에 구조화된 상세 정보를 첨부**할 수 있다는 것이다.

### WithDetails

```go
st := status.New(codes.InvalidArgument, "invalid request")
st, _ = st.WithDetails(
    &errdetails.BadRequest{
        FieldViolations: []*errdetails.BadRequest_FieldViolation{
            {Field: "email", Description: "invalid email format"},
            {Field: "age", Description: "must be positive"},
        },
    },
    &errdetails.RetryInfo{
        RetryDelay: durationpb.New(5 * time.Second),
    },
)
return nil, st.Err()
```

### Details 읽기

```go
st := status.Convert(err)
for _, detail := range st.Details() {
    switch d := detail.(type) {
    case *errdetails.BadRequest:
        for _, v := range d.GetFieldViolations() {
            log.Printf("필드 %s: %s", v.GetField(), v.GetDescription())
        }
    case *errdetails.RetryInfo:
        delay := d.GetRetryDelay().AsDuration()
        log.Printf("%v 후 재시도", delay)
    }
}
```

### 전송 메커니즘

```
Status + Details 전송 경로:
┌──────────────────────────────────────┐
│ Status{code, message, details}       │
└──────────┬───────────────────────────┘
           │
    ┌──────┴──────┐
    │  직렬화      │
    │  (protobuf) │
    └──────┬──────┘
           │
    HTTP/2 트레일러:
    ├── grpc-status: 3                    (코드)
    ├── grpc-message: invalid request     (메시지)
    └── grpc-status-details-bin: <base64> (details protobuf)
```

### 표준 Error Detail 타입

Google이 정의한 표준 에러 상세 타입 (`google/rpc/error_details.proto`):

| 타입 | 용도 |
|------|------|
| `RetryInfo` | 재시도 대기 시간 |
| `DebugInfo` | 스택 트레이스, 디버그 정보 |
| `QuotaFailure` | 쿼터 위반 상세 |
| `ErrorInfo` | 에러 원인, 도메인, 메타데이터 |
| `PreconditionFailure` | 전제조건 위반 상세 |
| `BadRequest` | 필드별 유효성 검증 실패 |
| `RequestInfo` | 요청 식별 정보 |
| `ResourceInfo` | 리소스 식별 정보 |
| `Help` | 도움말 링크 |
| `LocalizedMessage` | 다국어 에러 메시지 |

---

## 5. 프레임워크가 자동 생성하는 에러

### gRPC-Go가 자동 반환하는 상태 코드

| 상황 | 코드 | 메시지 예시 |
|------|------|-----------|
| context.Canceled | Canceled | `context canceled` |
| context.DeadlineExceeded | DeadlineExceeded | `context deadline exceeded` |
| 메서드 미등록 | Unimplemented | `unknown service xxx` |
| 메시지 크기 초과 | ResourceExhausted | `received message larger than max` |
| 서버 종료 | Unavailable | `transport is closing` |
| 연결 실패 | Unavailable | `connection error: ...` |
| 알 수 없는 인코딩 | Unimplemented | `unknown encoding: xxx` |
| 핸들러가 비-status 에러 반환 | Unknown | 에러 메시지 그대로 |
| GOAWAY 수신 | Unavailable | `the connection is draining` |
| RST_STREAM 수신 | Internal/Canceled | 스트림 종료 |

### 서버 processUnaryRPC에서의 에러 처리

```go
// server.go: processUnaryRPC 내부
reply, appErr := md.Handler(info.serviceImpl, ctx, df, s.opts.unaryInt)
if appErr != nil {
    // status.FromError(appErr) 시도
    // → GRPCStatus()가 있으면 해당 Status 사용
    // → 없으면 Unknown으로 래핑
    st, ok := status.FromError(appErr)
    if !ok {
        st = status.New(codes.Unknown, appErr.Error())
    }
    // WriteStatus로 클라이언트에 전송
}
```

---

## 6. 에러 처리 패턴

### 패턴 1: 상태 코드 기반 분기

```go
reply, err := client.GetUser(ctx, req)
if err != nil {
    switch status.Code(err) {
    case codes.NotFound:
        return nil, fmt.Errorf("사용자 없음: %s", req.GetId())
    case codes.PermissionDenied:
        return nil, fmt.Errorf("접근 권한 없음")
    case codes.Unavailable:
        // 재시도 가능
        return retry(ctx, req)
    default:
        return nil, fmt.Errorf("예상치 못한 에러: %v", err)
    }
}
```

### 패턴 2: 재시도 가능 여부 판단

```go
func isRetryable(err error) bool {
    code := status.Code(err)
    switch code {
    case codes.Unavailable, codes.ResourceExhausted, codes.Aborted:
        return true
    default:
        return false
    }
}
```

### 패턴 3: 서버에서 적절한 에러 반환

```go
func (s *server) GetUser(ctx context.Context, req *pb.GetUserReq) (*pb.User, error) {
    if req.GetId() == "" {
        return nil, status.Error(codes.InvalidArgument, "user_id is required")
    }

    user, err := s.db.FindUser(req.GetId())
    if err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            return nil, status.Errorf(codes.NotFound, "user %s not found", req.GetId())
        }
        return nil, status.Errorf(codes.Internal, "database error: %v", err)
    }

    return user, nil  // 성공 시 nil error → codes.OK
}
```

### 패턴 4: errors.Is/As 호환

```go
// gRPC Status 에러는 errors.Is와 호환됨
err := status.Error(codes.NotFound, "not found")
if errors.Is(err, context.Canceled) {
    // false — 다른 에러
}

// Status를 errors.As로 추출
var st *status.Status
if errors.As(err, &st) {
    // st.Code(), st.Message() 사용
}
```

---

## 7. HTTP 상태 코드 매핑

gRPC 코드와 HTTP 상태 코드는 다르지만, gRPC-gateway 등에서 매핑이 필요하다.

| gRPC 코드 | HTTP 상태 | 비고 |
|-----------|----------|------|
| OK (0) | 200 OK | |
| Canceled (1) | 499 Client Closed | 비표준 |
| Unknown (2) | 500 Internal | |
| InvalidArgument (3) | 400 Bad Request | |
| DeadlineExceeded (4) | 504 Gateway Timeout | |
| NotFound (5) | 404 Not Found | |
| AlreadyExists (6) | 409 Conflict | |
| PermissionDenied (7) | 403 Forbidden | |
| ResourceExhausted (8) | 429 Too Many Requests | |
| FailedPrecondition (9) | 400 Bad Request | |
| Aborted (10) | 409 Conflict | |
| OutOfRange (11) | 400 Bad Request | |
| Unimplemented (12) | 501 Not Implemented | |
| Internal (13) | 500 Internal | |
| Unavailable (14) | 503 Service Unavailable | |
| DataLoss (15) | 500 Internal | |
| Unauthenticated (16) | 401 Unauthorized | |

---

## 8. 코드 문자열 변환

```go
// codes/code_string.go
func (c Code) String() string {
    switch c {
    case OK:
        return "OK"
    case Canceled:
        return "Canceled"
    // ...
    }
}

// 역변환: 문자열 → 코드
func (c *Code) UnmarshalJSON(b []byte) error {
    // 숫자 또는 문자열 모두 지원
    // "3" 또는 "\"InvalidArgument\""
}
```

---

## 9. 재시도와 상태 코드

gRPC-Go의 자동 재시도는 Service Config의 `retryPolicy.retryableStatusCodes`로 설정한다.

```json
{
    "retryPolicy": {
        "maxAttempts": 3,
        "retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED"]
    }
}
```

**재시도 안전 코드**: Unavailable, ResourceExhausted
**재시도 위험 코드**: Internal (서버 버그 가능), Unknown (원인 불명)
**재시도 금지 코드**: InvalidArgument, NotFound, PermissionDenied (재시도해도 동일)

---

## 10. 종합 에러 흐름 다이어그램

```
서버 핸들러
    │
    ├── return nil, status.Error(codes.NotFound, "msg")
    │       │
    │       ▼
    │   processUnaryRPC
    │       │
    │       ├── status.FromError(err)
    │       │   → Status{code=5, message="msg"}
    │       │
    │       ├── WriteStatus(stream, st)
    │       │   → HTTP/2 트레일러 프레임
    │       │     grpc-status: 5
    │       │     grpc-message: msg
    │       │     grpc-status-details-bin: (있으면)
    │       │
    │       └── statsHandler.HandleRPC(End{Error: err})
    │
    ▼ (네트워크)
    │
클라이언트
    │
    ├── RecvMsg() → 트레일러 파싱
    │       │
    │       ├── grpc-status → codes.Code
    │       ├── grpc-message → string
    │       └── grpc-status-details-bin → []proto.Message
    │
    ├── Status 재구성
    │   → Status{code=5, message="msg", details=[...]}
    │
    └── st.Err() → error 반환
        → status.Code(err) == codes.NotFound
```

---

## 11. 모범 사례

### DO

```go
// 1. 적절한 코드 선택
return nil, status.Error(codes.InvalidArgument, "email format invalid")

// 2. Details로 구조화된 정보 제공
st, _ := status.New(codes.InvalidArgument, "validation failed").WithDetails(...)

// 3. 재시도 가능 여부 명확히 표현
return nil, status.Error(codes.Unavailable, "try again later")  // 재시도 OK

// 4. 에러 메시지에 사용자 입력 그대로 넣지 않기 (보안)
return nil, status.Errorf(codes.NotFound, "resource not found")  // ID 생략
```

### DON'T

```go
// 1. 모든 에러에 Internal 사용하지 않기
return nil, status.Error(codes.Internal, err.Error())  // BAD

// 2. 일반 error 반환하지 않기 (Unknown으로 변환됨)
return nil, fmt.Errorf("something failed")  // BAD → codes.Unknown

// 3. OK 코드로 에러 반환하지 않기
return nil, status.Error(codes.OK, "error")  // BAD → nil로 변환됨

// 4. 에러 메시지에 민감 정보 넣지 않기
return nil, status.Errorf(codes.Internal, "DB password: %s", pwd)  // BAD
```
