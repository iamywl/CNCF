// poc-10-metadata: gRPC 메타데이터(헤더/트레일러) 전파 시뮬레이션
//
// grpc-go의 metadata 패키지 핵심 개념을 표준 라이브러리만으로 재현한다.
// - MD 타입 (map[string][]string)
// - context.Context에 메타데이터 저장/추출
// - 클라이언트 → 서버 헤더 전파
// - 서버 → 클라이언트 헤더/트레일러 전파
// - 바이너리 헤더 (-bin 접미사, base64 인코딩)
//
// 실제 grpc-go 소스: metadata/metadata.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ========== MD 타입 ==========
// grpc-go: metadata/metadata.go 45행
// MD는 gRPC 메타데이터의 핵심 타입이다. HTTP/2 헤더와 1:1 대응된다.
// 키는 소문자 정규화, 값은 복수개 가능.
type MD map[string][]string

// New는 key-value 쌍으로 MD를 생성한다.
func New(kv map[string]string) MD {
	md := MD{}
	for k, v := range kv {
		key := strings.ToLower(k)
		md[key] = append(md[key], v)
	}
	return md
}

// Pairs는 key, value 쌍의 가변 인자로 MD를 생성한다.
// grpc-go: metadata/metadata.go Pairs 함수
func Pairs(kv ...string) MD {
	if len(kv)%2 != 0 {
		panic("metadata: Pairs는 짝수 개의 인자가 필요합니다")
	}
	md := MD{}
	for i := 0; i < len(kv); i += 2 {
		key := strings.ToLower(kv[i])
		md[key] = append(md[key], kv[i+1])
	}
	return md
}

// Get은 키에 해당하는 값을 반환한다.
func (md MD) Get(key string) []string {
	return md[strings.ToLower(key)]
}

// Set은 키에 값을 설정한다 (기존 값 덮어쓰기).
func (md MD) Set(key string, vals ...string) {
	md[strings.ToLower(key)] = vals
}

// Append는 키에 값을 추가한다 (기존 값 유지).
func (md MD) Append(key string, vals ...string) {
	key = strings.ToLower(key)
	md[key] = append(md[key], vals...)
}

// Join은 두 MD를 병합한다.
func Join(mds ...MD) MD {
	out := MD{}
	for _, md := range mds {
		for k, v := range md {
			out[k] = append(out[k], v...)
		}
	}
	return out
}

// Copy는 MD를 복사한다.
func (md MD) Copy() MD {
	out := MD{}
	for k, v := range md {
		out[k] = make([]string, len(v))
		copy(out[k], v)
	}
	return out
}

// Len은 키 개수를 반환한다.
func (md MD) Len() int {
	return len(md)
}

// String은 MD를 보기 좋게 출력한다.
func (md MD) String() string {
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		for _, v := range md[k] {
			// -bin 접미사: base64 디코딩하여 표시
			if isBinHeader(k) {
				decoded, err := base64.StdEncoding.DecodeString(v)
				if err == nil {
					sb.WriteString(fmt.Sprintf("  %s: [binary %d bytes]\n", k, len(decoded)))
					continue
				}
			}
			sb.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
	}
	return sb.String()
}

// ========== 바이너리 헤더 ==========
// grpc-go: "-bin" 접미사가 붙은 키는 바이너리 데이터로 간주하여
// base64 인코딩으로 전송한다. 수신 측에서 자동 디코딩한다.
func isBinHeader(key string) bool {
	return strings.HasSuffix(key, "-bin")
}

// encodeBinHeader는 바이너리 데이터를 base64로 인코딩한다.
func encodeBinHeader(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// decodeBinHeader는 base64 문자열을 바이너리로 디코딩한다.
func decodeBinHeader(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// ========== Context 통합 ==========
// grpc-go: metadata/metadata.go — context에 메타데이터를 저장하는 방식
// 키 타입을 별도로 정의하여 context 충돌을 방지한다.
type mdContextKey struct{ kind string }

var (
	mdOutgoingKey = mdContextKey{"outgoing"} // 클라이언트가 보낼 메타데이터
	mdIncomingKey = mdContextKey{"incoming"} // 서버가 수신한 메타데이터
)

// NewOutgoingContext는 보낼 메타데이터를 context에 저장한다.
// grpc-go: metadata/metadata.go NewOutgoingContext
func NewOutgoingContext(ctx context.Context, md MD) context.Context {
	return context.WithValue(ctx, mdOutgoingKey, md)
}

// FromOutgoingContext는 context에서 보낼 메타데이터를 추출한다.
func FromOutgoingContext(ctx context.Context) (MD, bool) {
	md, ok := ctx.Value(mdOutgoingKey).(MD)
	return md, ok
}

// NewIncomingContext는 수신한 메타데이터를 context에 저장한다.
// grpc-go: metadata/metadata.go NewIncomingContext
func NewIncomingContext(ctx context.Context, md MD) context.Context {
	return context.WithValue(ctx, mdIncomingKey, md)
}

// FromIncomingContext는 context에서 수신한 메타데이터를 추출한다.
func FromIncomingContext(ctx context.Context) (MD, bool) {
	md, ok := ctx.Value(mdIncomingKey).(MD)
	return md, ok
}

// AppendToOutgoingContext는 기존 outgoing MD에 key-value를 추가한다.
// grpc-go: metadata/metadata.go AppendToOutgoingContext
func AppendToOutgoingContext(ctx context.Context, kv ...string) context.Context {
	md, ok := FromOutgoingContext(ctx)
	if !ok {
		md = MD{}
	}
	md = md.Copy()
	newMD := Pairs(kv...)
	for k, v := range newMD {
		md[k] = append(md[k], v...)
	}
	return NewOutgoingContext(ctx, md)
}

// ========== RPC 시뮬레이션 ==========

// ServerResponse는 서버 → 클라이언트로 전달되는 응답 메타데이터
type ServerResponse struct {
	Header  MD
	Trailer MD
	Body    string
}

// simulateUnaryRPC는 Unary RPC의 메타데이터 전파를 시뮬레이션한다.
func simulateUnaryRPC(ctx context.Context, method string, handler func(context.Context) ServerResponse) ServerResponse {
	fmt.Printf("\n  ── RPC 호출: %s ──\n", method)

	// 클라이언트 측: outgoing 메타데이터를 HTTP/2 헤더로 전송
	if md, ok := FromOutgoingContext(ctx); ok {
		fmt.Println("  [클라이언트 → 서버] 요청 헤더:")
		fmt.Print(md.String())

		// 서버 측: 수신한 메타데이터를 incoming context에 저장
		ctx = NewIncomingContext(ctx, md)
	}

	// 서버 핸들러 실행
	resp := handler(ctx)

	// 서버 → 클라이언트 응답 헤더/트레일러
	if resp.Header.Len() > 0 {
		fmt.Println("  [서버 → 클라이언트] 응답 헤더:")
		fmt.Print(resp.Header.String())
	}
	if resp.Trailer.Len() > 0 {
		fmt.Println("  [서버 → 클라이언트] 트레일러:")
		fmt.Print(resp.Trailer.String())
	}

	return resp
}

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Metadata 시뮬레이션")
	fmt.Println("========================================")

	// 1. MD 기본 연산
	fmt.Println("\n[1] MD 기본 연산")
	fmt.Println("──────────────────")

	md1 := New(map[string]string{
		"Authorization": "Bearer token-abc",
		"X-Request-Id":  "req-12345",
	})
	fmt.Println("  New()로 생성:")
	fmt.Print(md1.String())

	md2 := Pairs(
		"user-agent", "grpc-go/1.60.0",
		"content-type", "application/grpc",
		"x-custom", "value1",
		"x-custom", "value2",
	)
	fmt.Println("  Pairs()로 생성:")
	fmt.Print(md2.String())

	merged := Join(md1, md2)
	fmt.Printf("  Join() 결과: %d개 키\n", merged.Len())

	// 2. 바이너리 헤더
	fmt.Println("\n[2] 바이너리 헤더 (-bin 접미사)")
	fmt.Println("───────────────────────────────")

	binaryData := []byte{0x08, 0x96, 0x01, 0x12, 0x0a, 0x48, 0x65, 0x6c, 0x6c, 0x6f}
	encoded := encodeBinHeader(binaryData)
	fmt.Printf("  원본 바이너리: %x (%d bytes)\n", binaryData, len(binaryData))
	fmt.Printf("  base64 인코딩: %s\n", encoded)

	binMD := Pairs(
		"trace-proto-bin", encoded,
		"normal-header", "text-value",
	)
	fmt.Println("  바이너리 포함 MD:")
	fmt.Print(binMD.String())

	decoded, _ := decodeBinHeader(binMD.Get("trace-proto-bin")[0])
	fmt.Printf("  디코딩 결과: %x (원본과 동일: %v)\n",
		decoded, fmt.Sprintf("%x", decoded) == fmt.Sprintf("%x", binaryData))

	// 3. Context에 메타데이터 저장/추출
	fmt.Println("\n[3] Context 기반 메타데이터 전파")
	fmt.Println("─────────────────────────────────")

	ctx := context.Background()

	// 클라이언트: outgoing 메타데이터 설정
	ctx = NewOutgoingContext(ctx, Pairs(
		"authorization", "Bearer my-token",
		"x-request-id", fmt.Sprintf("req-%d", time.Now().UnixMilli()),
	))

	// 추가 메타데이터 append
	ctx = AppendToOutgoingContext(ctx,
		"x-trace-id", "trace-abc-123",
		"x-custom-header", "custom-value",
	)

	if md, ok := FromOutgoingContext(ctx); ok {
		fmt.Println("  Outgoing 메타데이터:")
		fmt.Print(md.String())
	}

	// 4. Unary RPC 전체 흐름
	fmt.Println("\n[4] Unary RPC 메타데이터 전파 흐름")
	fmt.Println("────────────────────────────────────")
	fmt.Println("  클라이언트 → HEADERS(메타데이터) → 서버")
	fmt.Println("  서버 → HEADERS(응답헤더) + DATA + HEADERS(트레일러) → 클라이언트")

	resp := simulateUnaryRPC(ctx, "/myservice.MyService/GetUser",
		func(ctx context.Context) ServerResponse {
			// 서버 핸들러: incoming 메타데이터 읽기
			if md, ok := FromIncomingContext(ctx); ok {
				fmt.Println("  [서버] 수신한 메타데이터에서 인증 토큰 확인:")
				if auth := md.Get("authorization"); len(auth) > 0 {
					fmt.Printf("    authorization: %s\n", auth[0])
				}
				if trace := md.Get("x-trace-id"); len(trace) > 0 {
					fmt.Printf("    x-trace-id: %s\n", trace[0])
				}
			}

			// 서버: 응답 헤더와 트레일러 설정
			return ServerResponse{
				Header: Pairs(
					"x-response-id", "resp-98765",
					"x-server-region", "ap-northeast-2",
				),
				Trailer: Pairs(
					"x-rpc-status", "OK",
					"x-processing-time-ms", "42",
					"x-request-count-bin", encodeBinHeader([]byte{0x00, 0x00, 0x01, 0xF4}),
				),
				Body: `{"user_id": 1, "name": "test"}`,
			}
		},
	)
	fmt.Printf("  응답 본문: %s\n", resp.Body)

	// 5. 스트리밍 RPC에서의 메타데이터 타이밍
	fmt.Println("\n[5] 스트리밍 RPC 메타데이터 타이밍")
	fmt.Println("───────────────────────────────────")
	fmt.Println("  Unary RPC:")
	fmt.Println("    요청:  HEADERS(메타데이터) → DATA(요청) → END_STREAM")
	fmt.Println("    응답:  HEADERS(응답헤더) → DATA(응답) → HEADERS(트레일러)+END_STREAM")
	fmt.Println()
	fmt.Println("  Server Streaming:")
	fmt.Println("    요청:  HEADERS → DATA → END_STREAM")
	fmt.Println("    응답:  HEADERS → DATA → DATA → ... → HEADERS(트레일러)+END_STREAM")
	fmt.Println("    ※ 응답 헤더는 첫 번째 DATA 전에 전송")
	fmt.Println()
	fmt.Println("  Client Streaming:")
	fmt.Println("    요청:  HEADERS → DATA → DATA → ... → END_STREAM")
	fmt.Println("    응답:  HEADERS → DATA → HEADERS(트레일러)+END_STREAM")
	fmt.Println()
	fmt.Println("  Bidirectional Streaming:")
	fmt.Println("    요청:  HEADERS → DATA → DATA → ...")
	fmt.Println("    응답:  HEADERS → DATA → DATA → ... → HEADERS(트레일러)+END_STREAM")

	// 6. 예약된 메타데이터 키
	fmt.Println("\n[6] 예약된 메타데이터 키 (grpc- 접두사)")
	fmt.Println("────────────────────────────────────────")
	reserved := map[string]string{
		"grpc-timeout":          "요청 타임아웃 (예: 1S, 100m)",
		"grpc-encoding":         "압축 방식 (gzip, identity)",
		"grpc-accept-encoding":  "수용 가능한 압축 방식",
		"grpc-status":           "응답 상태 코드 (트레일러)",
		"grpc-message":          "상태 메시지 (트레일러)",
		"grpc-status-details-bin": "상세 에러 정보 (protobuf, 트레일러)",
		":authority":            "HTTP/2 의사 헤더 — 대상 서버",
		":method":               "HTTP/2 의사 헤더 — POST (항상)",
		":path":                 "HTTP/2 의사 헤더 — /service/method",
		":scheme":               "HTTP/2 의사 헤더 — http 또는 https",
	}
	for k, v := range reserved {
		fmt.Printf("  %-28s → %s\n", k, v)
	}

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
