// poc-11-status-codes: gRPC 에러 처리 및 상태 코드 시뮬레이션
//
// grpc-go의 status/codes 패키지 핵심 개념을 표준 라이브러리만으로 재현한다.
// - 17개 gRPC 상태 코드 정의
// - Status 구조체 (code, message, details)
// - Error ↔ Status 변환
// - 에러 분기 처리 (코드별 다른 동작)
// - WithDetails 패턴
//
// 실제 grpc-go 소스: internal/status/status.go, codes/codes.go
package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ========== 상태 코드 ==========
// grpc-go: codes/codes.go
// gRPC는 17개의 표준 상태 코드를 정의한다. HTTP 상태 코드와 다르게
// RPC 의미론에 맞게 설계되었다.
type Code uint32

const (
	OK                 Code = 0  // 성공
	Canceled           Code = 1  // 클라이언트가 취소
	Unknown            Code = 2  // 알 수 없는 에러
	InvalidArgument    Code = 3  // 잘못된 인자
	DeadlineExceeded   Code = 4  // 기한 초과
	NotFound           Code = 5  // 리소스 없음
	AlreadyExists      Code = 6  // 이미 존재
	PermissionDenied   Code = 7  // 권한 거부
	ResourceExhausted  Code = 8  // 리소스 소진 (예: 쿼터)
	FailedPrecondition Code = 9  // 전제조건 실패
	Aborted            Code = 10 // 트랜잭션 충돌 등으로 중단
	OutOfRange         Code = 11 // 범위 초과
	Unimplemented      Code = 12 // 구현되지 않음
	Internal           Code = 13 // 내부 서버 오류
	Unavailable        Code = 14 // 서비스 불가 (일시적)
	DataLoss           Code = 15 // 데이터 유실
	Unauthenticated    Code = 16 // 인증 실패
)

var codeNames = map[Code]string{
	OK:                 "OK",
	Canceled:           "CANCELLED",
	Unknown:            "UNKNOWN",
	InvalidArgument:    "INVALID_ARGUMENT",
	DeadlineExceeded:   "DEADLINE_EXCEEDED",
	NotFound:           "NOT_FOUND",
	AlreadyExists:      "ALREADY_EXISTS",
	PermissionDenied:   "PERMISSION_DENIED",
	ResourceExhausted:  "RESOURCE_EXHAUSTED",
	FailedPrecondition: "FAILED_PRECONDITION",
	Aborted:            "ABORTED",
	OutOfRange:         "OUT_OF_RANGE",
	Unimplemented:      "UNIMPLEMENTED",
	Internal:           "INTERNAL",
	Unavailable:        "UNAVAILABLE",
	DataLoss:           "DATA_LOSS",
	Unauthenticated:    "UNAUTHENTICATED",
}

func (c Code) String() string {
	if name, ok := codeNames[c]; ok {
		return name
	}
	return fmt.Sprintf("CODE(%d)", c)
}

// ========== HTTP ↔ gRPC 코드 매핑 ==========
// grpc-go: codes/codes.go — HTTP 상태 코드와 gRPC 코드 간 변환
func HTTPStatusFromCode(code Code) int {
	switch code {
	case OK:
		return 200
	case Canceled:
		return 499
	case InvalidArgument:
		return 400
	case DeadlineExceeded:
		return 504
	case NotFound:
		return 404
	case AlreadyExists:
		return 409
	case PermissionDenied:
		return 403
	case ResourceExhausted:
		return 429
	case Unimplemented:
		return 501
	case Unavailable:
		return 503
	case Unauthenticated:
		return 401
	default:
		return 500
	}
}

// ========== Detail ==========
// grpc-go는 google.protobuf.Any를 사용하여 에러 세부 정보를 전달한다.
// 여기서는 단순 문자열 기반으로 시뮬레이션한다.
type Detail struct {
	TypeURL string // 타입 식별자 (예: "type.googleapis.com/google.rpc.BadRequest")
	Message string // 상세 메시지
}

func (d Detail) String() string {
	return fmt.Sprintf("{type=%s, msg=%s}", d.TypeURL, d.Message)
}

// ========== Status 구조체 ==========
// grpc-go: internal/status/status.go 43행
// Status는 gRPC 에러의 핵심 타입이다. protobuf의 google.rpc.Status에 대응한다.
type Status struct {
	code    Code
	message string
	details []Detail
}

// New는 코드와 메시지로 Status를 생성한다.
func New(code Code, msg string) *Status {
	return &Status{code: code, message: msg}
}

// Newf는 포맷 문자열로 Status를 생성한다.
func Newf(code Code, format string, args ...interface{}) *Status {
	return &Status{code: code, message: fmt.Sprintf(format, args...)}
}

// Err는 Status를 error로 변환한다. OK이면 nil을 반환한다.
// grpc-go: internal/status/status.go Err()
func (s *Status) Err() error {
	if s.code == OK {
		return nil
	}
	return &StatusError{s: s}
}

// Errorf는 코드와 포맷 문자열로 바로 error를 생성한다.
func Errorf(code Code, format string, args ...interface{}) error {
	return Newf(code, format, args...).Err()
}

// WithDetails는 Status에 상세 정보를 추가한 새 Status를 반환한다.
// grpc-go: internal/status/status.go WithDetails()
func (s *Status) WithDetails(details ...Detail) *Status {
	newStatus := &Status{
		code:    s.code,
		message: s.message,
		details: make([]Detail, len(s.details)+len(details)),
	}
	copy(newStatus.details, s.details)
	copy(newStatus.details[len(s.details):], details)
	return newStatus
}

// Code는 상태 코드를 반환한다.
func (s *Status) Code() Code { return s.code }

// Message는 상태 메시지를 반환한다.
func (s *Status) Message() string { return s.message }

// Details는 상세 정보를 반환한다.
func (s *Status) Details() []Detail { return s.details }

// String은 Status를 문자열로 표현한다.
func (s *Status) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("rpc error: code = %s desc = %s", s.code, s.message))
	if len(s.details) > 0 {
		sb.WriteString("\n  details:")
		for _, d := range s.details {
			sb.WriteString(fmt.Sprintf("\n    - %s", d))
		}
	}
	return sb.String()
}

// ========== StatusError ==========
// grpc-go: internal/status/status.go — Error() 반환 타입
// error 인터페이스를 구현하면서 Status 정보를 보존한다.
type StatusError struct {
	s *Status
}

func (e *StatusError) Error() string {
	return e.s.String()
}

// GRPCStatus는 StatusError에서 Status를 추출한다.
// grpc-go: 이 메서드를 통해 error → Status 변환이 가능하다.
func (e *StatusError) GRPCStatus() *Status {
	return e.s
}

// ========== Error → Status 변환 ==========
// grpc-go: status/status.go FromError
// 일반 error를 Status로 변환한다. StatusError이면 직접 추출하고,
// 아니면 Unknown 코드로 래핑한다.
func FromError(err error) *Status {
	if err == nil {
		return New(OK, "")
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.GRPCStatus()
	}
	// 일반 error → Unknown 코드
	return New(Unknown, err.Error())
}

// ========== 서비스 시뮬레이션 ==========

type UserService struct{}

func (s *UserService) GetUser(userID string) (string, error) {
	switch userID {
	case "":
		return "", Newf(InvalidArgument, "user_id는 필수입니다").
			WithDetails(Detail{
				TypeURL: "type.googleapis.com/google.rpc.BadRequest",
				Message: "field: user_id, description: 비어있음",
			}).Err()
	case "deleted-user":
		return "", Newf(NotFound, "사용자 %q을(를) 찾을 수 없습니다", userID).Err()
	case "banned-user":
		return "", Newf(PermissionDenied, "차단된 사용자입니다").
			WithDetails(
				Detail{TypeURL: "type.googleapis.com/google.rpc.ErrorInfo",
					Message: "reason: USER_BANNED, domain: myservice.com"},
				Detail{TypeURL: "type.googleapis.com/google.rpc.Help",
					Message: "관리자에게 문의하세요: admin@myservice.com"},
			).Err()
	case "slow-user":
		return "", Newf(DeadlineExceeded, "처리 시간 초과 (timeout=1s)").Err()
	case "overloaded":
		return "", Newf(ResourceExhausted, "요청 한도 초과: 분당 100회").Err()
	case "crash":
		return "", fmt.Errorf("nil pointer dereference at user.go:42")
	default:
		return fmt.Sprintf(`{"id": "%s", "name": "홍길동"}`, userID), nil
	}
}

// handleError는 gRPC 상태 코드에 따라 다른 에러 처리를 수행한다.
// 이것이 gRPC 에러 처리의 핵심 패턴이다.
func handleError(err error) {
	st := FromError(err)
	fmt.Printf("  상태: %s (HTTP %d)\n", st.Code(), HTTPStatusFromCode(st.Code()))
	fmt.Printf("  메시지: %s\n", st.Message())

	if details := st.Details(); len(details) > 0 {
		fmt.Println("  상세 정보:")
		for _, d := range details {
			fmt.Printf("    - [%s] %s\n", d.TypeURL, d.Message)
		}
	}

	// 코드별 클라이언트 동작 분기
	switch st.Code() {
	case InvalidArgument:
		fmt.Println("  → 동작: 요청 파라미터 수정 후 재시도")
	case NotFound:
		fmt.Println("  → 동작: 사용자에게 '없음' 표시")
	case PermissionDenied:
		fmt.Println("  → 동작: 접근 거부 페이지 표시")
	case DeadlineExceeded:
		fmt.Println("  → 동작: 타임아웃 증가 후 재시도")
	case ResourceExhausted:
		fmt.Println("  → 동작: 백오프 후 재시도")
	case Unavailable:
		fmt.Println("  → 동작: 즉시 재시도 (일시적 오류)")
	case Unknown, Internal:
		fmt.Println("  → 동작: 에러 로깅, 일반 오류 메시지 표시")
	case Unauthenticated:
		fmt.Println("  → 동작: 재인증 필요")
	default:
		fmt.Println("  → 동작: 기본 에러 처리")
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Status Codes 시뮬레이션")
	fmt.Println("========================================")

	// 1. 17개 상태 코드 목록
	fmt.Println("\n[1] gRPC 상태 코드 전체 목록")
	fmt.Println("─────────────────────────────")
	allCodes := []Code{OK, Canceled, Unknown, InvalidArgument, DeadlineExceeded,
		NotFound, AlreadyExists, PermissionDenied, ResourceExhausted,
		FailedPrecondition, Aborted, OutOfRange, Unimplemented,
		Internal, Unavailable, DataLoss, Unauthenticated}
	for _, c := range allCodes {
		retryable := "N"
		if c == Unavailable || c == ResourceExhausted || c == Aborted || c == DeadlineExceeded {
			retryable = "Y"
		}
		fmt.Printf("  %2d %-22s HTTP=%3d 재시도=%s\n", c, c.String(), HTTPStatusFromCode(c), retryable)
	}

	// 2. Status 생성 및 변환
	fmt.Println("\n[2] Status 생성 및 Error 변환")
	fmt.Println("───────────────────────────────")

	// Status → Error
	st := Newf(NotFound, "리소스를 찾을 수 없습니다: user/123")
	err := st.Err()
	fmt.Printf("  Status → Error: %v\n", err)

	// Error → Status
	recovered := FromError(err)
	fmt.Printf("  Error → Status: code=%s, msg=%s\n", recovered.Code(), recovered.Message())

	// 일반 error → Status (Unknown으로 래핑)
	plainErr := fmt.Errorf("connection refused")
	unknownSt := FromError(plainErr)
	fmt.Printf("  일반 error → Status: code=%s, msg=%s\n", unknownSt.Code(), unknownSt.Message())

	// OK → nil
	okErr := New(OK, "").Err()
	fmt.Printf("  OK Status → Error: %v (nil 반환)\n", okErr)

	// 3. WithDetails 패턴
	fmt.Println("\n[3] WithDetails 패턴")
	fmt.Println("─────────────────────")

	detailed := Newf(InvalidArgument, "잘못된 요청").
		WithDetails(
			Detail{TypeURL: "type.googleapis.com/google.rpc.BadRequest",
				Message: "field_violations: [{field: 'email', description: '형식 오류'}]"},
			Detail{TypeURL: "type.googleapis.com/google.rpc.DebugInfo",
				Message: "stack_trace: validator.go:128"},
		)
	fmt.Printf("  %s\n", detailed.String())

	// 4. 서비스 호출 시뮬레이션 — 코드별 에러 처리
	fmt.Println("\n[4] 서비스 호출 시뮬레이션")
	fmt.Println("──────────────────────────")

	svc := &UserService{}
	testCases := []struct {
		name   string
		userID string
	}{
		{"정상 요청", "user-1"},
		{"빈 ID", ""},
		{"삭제된 사용자", "deleted-user"},
		{"차단된 사용자", "banned-user"},
		{"느린 사용자", "slow-user"},
		{"과부하", "overloaded"},
		{"내부 오류", "crash"},
	}

	for _, tc := range testCases {
		fmt.Printf("\n  ── %s (userID=%q) ──\n", tc.name, tc.userID)
		result, err := svc.GetUser(tc.userID)
		if err != nil {
			handleError(err)
		} else {
			fmt.Printf("  상태: OK\n")
			fmt.Printf("  결과: %s\n", result)
		}
	}

	// 5. 재시도 가능한 코드 판별
	fmt.Println("\n[5] 재시도 정책 시뮬레이션")
	fmt.Println("──────────────────────────")
	retryableErrors := []error{
		Errorf(Unavailable, "서비스 일시 불가"),
		Errorf(ResourceExhausted, "쿼터 초과"),
		Errorf(PermissionDenied, "권한 없음"),
		Errorf(Internal, "내부 오류"),
	}

	for _, err := range retryableErrors {
		st := FromError(err)
		canRetry := st.Code() == Unavailable || st.Code() == ResourceExhausted
		action := "재시도 안 함"
		if canRetry {
			action = "재시도 가능 (백오프)"
		}
		fmt.Printf("  %-22s → %s\n", st.Code(), action)
	}

	// 6. grpc-status 트레일러 형식
	fmt.Println("\n[6] HTTP/2 트레일러 형식")
	fmt.Println("────────────────────────")
	st = Newf(NotFound, "user not found: 사용자 없음")
	fmt.Println("  실제 HTTP/2 트레일러로 전송되는 형태:")
	fmt.Printf("    grpc-status: %d\n", st.Code())
	fmt.Printf("    grpc-message: %s\n", st.Message())
	// details는 grpc-status-details-bin 트레일러로 protobuf 직렬화하여 전송
	timestamp := time.Now().Format(time.RFC3339)
	fmt.Printf("    grpc-status-details-bin: [base64 encoded protobuf, timestamp=%s]\n", timestamp)

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
