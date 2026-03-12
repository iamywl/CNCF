// poc-01-plugin-chain: CoreDNS 플러그인 체인 시뮬레이션
//
// CoreDNS의 핵심 설계 패턴인 Plugin func(Handler) Handler를 재현한다.
// 실제 CoreDNS 소스코드(plugin/plugin.go)의 아래 구조를 모방:
//   - Handler 인터페이스: ServeDNS 메서드
//   - Plugin 타입: func(Handler) Handler (미들웨어 패턴)
//   - 체인 구축: 역순 루프로 wrapping (core/dnsserver/server.go:105)
//   - NextOrFailure: 다음 플러그인 호출 또는 실패 반환
//
// 사용법: go run main.go

package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// 핵심 타입 정의 (CoreDNS plugin/plugin.go 재현)
// =============================================================================

// Handler는 CoreDNS의 plugin.Handler 인터페이스를 재현한다.
// 실제 CoreDNS에서는 ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
// 여기서는 단순화하여 (name, qtype) → (response, error)로 표현한다.
type Handler interface {
	ServeDNS(name string, qtype string) (string, error)
	Name() string
}

// Plugin은 CoreDNS의 plugin.Plugin 타입을 재현한다.
// 실제: type Plugin func(Handler) Handler
// 핸들러를 받아서 래핑된 핸들러를 반환하는 미들웨어 패턴이다.
type Plugin func(Handler) Handler

// RcodeServerFailure는 DNS SERVFAIL 응답 코드를 나타낸다.
const RcodeServerFailure = "SERVFAIL"

// NextOrFailure는 CoreDNS의 plugin.NextOrFailure를 재현한다.
// 실제 코드(plugin/plugin.go:74):
//
//	func NextOrFailure(name string, next Handler, ...) (int, error) {
//	    if next != nil {
//	        return next.ServeDNS(ctx, w, r)
//	    }
//	    return dns.RcodeServerFailure, Error(name, errors.New("no next plugin found"))
//	}
func NextOrFailure(name string, next Handler, qname string, qtype string) (string, error) {
	if next != nil {
		return next.ServeDNS(qname, qtype)
	}
	return RcodeServerFailure, fmt.Errorf("plugin/%s: no next plugin found", name)
}

// =============================================================================
// 플러그인 구현 1: LogPlugin (비터미널 - 로깅 후 다음 플러그인 호출)
// =============================================================================

// LogPlugin은 요청/응답을 로깅하는 플러그인이다.
// CoreDNS의 log 플러그인과 유사: 요청 정보를 출력하고 다음 플러그인으로 전달한다.
type LogPlugin struct {
	Next Handler // 다음 핸들러 참조
}

func (l *LogPlugin) ServeDNS(name string, qtype string) (string, error) {
	start := time.Now()
	fmt.Printf("  [LOG] ▶ 요청 수신: %s %s\n", name, qtype)

	// 다음 플러그인으로 전달 (NextOrFailure 패턴)
	response, err := NextOrFailure(l.Name(), l.Next, name, qtype)

	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("  [LOG] ◀ 오류 응답: %s (소요: %v)\n", err, elapsed)
	} else {
		fmt.Printf("  [LOG] ◀ 정상 응답: %s (소요: %v)\n", response, elapsed)
	}
	return response, err
}

func (l *LogPlugin) Name() string { return "log" }

// NewLogPlugin은 LogPlugin을 생성하는 Plugin 팩토리 함수이다.
// 실제 CoreDNS에서 각 플러그인은 setup.go에서 이런 팩토리를 등록한다.
func NewLogPlugin() Plugin {
	return func(next Handler) Handler {
		return &LogPlugin{Next: next}
	}
}

// =============================================================================
// 플러그인 구현 2: CachePlugin (조건부 - 캐시 히트 시 체인 중단)
// =============================================================================

// CachePlugin은 응답을 캐싱하는 플러그인이다.
// CoreDNS의 cache 플러그인과 유사: 캐시 히트 시 다음 플러그인 호출 없이 즉시 반환한다.
type CachePlugin struct {
	Next  Handler
	cache map[string]string // 단순화된 캐시: "name:qtype" → response
}

func (c *CachePlugin) ServeDNS(name string, qtype string) (string, error) {
	key := name + ":" + qtype

	// 캐시 히트: 체인을 더 이상 진행하지 않고 즉시 반환
	if resp, ok := c.cache[key]; ok {
		fmt.Printf("  [CACHE] ✓ 캐시 히트: %s → %s\n", key, resp)
		return resp, nil
	}

	fmt.Printf("  [CACHE] ✗ 캐시 미스: %s\n", key)

	// 캐시 미스: 다음 플러그인으로 전달
	response, err := NextOrFailure(c.Name(), c.Next, name, qtype)

	// 정상 응답이면 캐시에 저장
	if err == nil {
		c.cache[key] = response
		fmt.Printf("  [CACHE] 저장: %s → %s\n", key, response)
	}

	return response, err
}

func (c *CachePlugin) Name() string { return "cache" }

// NewCachePlugin은 CachePlugin을 생성하는 Plugin 팩토리 함수이다.
func NewCachePlugin() Plugin {
	return func(next Handler) Handler {
		return &CachePlugin{
			Next:  next,
			cache: make(map[string]string),
		}
	}
}

// =============================================================================
// 플러그인 구현 3: EchoPlugin (터미널 - 체인의 마지막, 실제 응답 생성)
// =============================================================================

// EchoPlugin은 요청 이름을 그대로 응답하는 터미널 플러그인이다.
// CoreDNS의 whoami 플러그인과 유사: 다음 플러그인을 호출하지 않고 직접 응답한다.
type EchoPlugin struct {
	Next    Handler            // 터미널이지만 Next 필드는 존재 (fallthrough 용)
	records map[string]string  // 미리 정의된 레코드
}

func (e *EchoPlugin) ServeDNS(name string, qtype string) (string, error) {
	key := name + ":" + qtype

	if resp, ok := e.records[key]; ok {
		fmt.Printf("  [ECHO] 레코드 발견: %s → %s\n", key, resp)
		return resp, nil
	}

	// 레코드가 없으면 다음 플러그인으로 fallthrough (CoreDNS fall 패턴)
	fmt.Printf("  [ECHO] 레코드 없음: %s, fallthrough\n", key)
	return NextOrFailure(e.Name(), e.Next, name, qtype)
}

func (e *EchoPlugin) Name() string { return "echo" }

// NewEchoPlugin은 EchoPlugin을 생성하는 Plugin 팩토리 함수이다.
func NewEchoPlugin(records map[string]string) Plugin {
	return func(next Handler) Handler {
		return &EchoPlugin{
			Next:    next,
			records: records,
		}
	}
}

// =============================================================================
// 플러그인 구현 4: ErrorsPlugin (오류 처리 플러그인)
// =============================================================================

// ErrorsPlugin은 하위 플러그인의 오류를 처리하는 플러그인이다.
// CoreDNS의 errors 플러그인과 유사.
type ErrorsPlugin struct {
	Next Handler
}

func (e *ErrorsPlugin) ServeDNS(name string, qtype string) (string, error) {
	response, err := NextOrFailure(e.Name(), e.Next, name, qtype)
	if err != nil {
		fmt.Printf("  [ERRORS] 오류 감지: %v\n", err)
		// 오류를 로깅하되 전파는 계속한다
	}
	return response, err
}

func (e *ErrorsPlugin) Name() string { return "errors" }

func NewErrorsPlugin() Plugin {
	return func(next Handler) Handler {
		return &ErrorsPlugin{Next: next}
	}
}

// =============================================================================
// 체인 구축 (core/dnsserver/server.go:103-128 재현)
// =============================================================================

// BuildChain은 플러그인 목록으로 체인을 구축한다.
// 실제 CoreDNS 코드:
//
//	var stack plugin.Handler
//	for i := len(site.Plugin) - 1; i >= 0; i-- {
//	    stack = site.Plugin[i](stack)
//	}
//	site.pluginChain = stack
//
// 역순 루프로 마지막 플러그인부터 wrapping하여
// 결과적으로 첫 번째 플러그인이 가장 바깥에 위치한다.
func BuildChain(plugins []Plugin) Handler {
	var stack Handler // nil로 시작 (체인의 끝)

	// 역순 루프: 마지막 플러그인부터 감싸기
	for i := len(plugins) - 1; i >= 0; i-- {
		stack = plugins[i](stack)
	}

	return stack
}

// =============================================================================
// ClientWrite는 응답 코드를 확인하여 클라이언트에 응답이 작성되었는지 판단한다.
// CoreDNS plugin/plugin.go:137의 ClientWrite 재현.
// =============================================================================

func ClientWrite(rcode string) bool {
	switch rcode {
	case RcodeServerFailure, "REFUSED", "FORMERR", "NOTIMP":
		return false
	}
	return true
}

// =============================================================================
// 메인: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== CoreDNS 플러그인 체인 시뮬레이션 ===")
	fmt.Println()

	// -------------------------------------------------------------------------
	// 1. 레코드 정의 (EchoPlugin이 사용할 데이터)
	// -------------------------------------------------------------------------
	records := map[string]string{
		"example.com.:A":     "93.184.216.34",
		"example.com.:AAAA":  "2606:2800:220:1:248:1893:25c8:1946",
		"www.example.com.:A": "93.184.216.34",
	}

	// -------------------------------------------------------------------------
	// 2. 플러그인 체인 구성 (실행 순서대로)
	// -------------------------------------------------------------------------
	// CoreDNS Corefile에서의 순서와 동일:
	//   errors → log → cache → echo
	//
	// BuildChain은 역순으로 wrapping하므로:
	//   errors(log(cache(echo(nil))))
	//
	// 요청 흐름: errors → log → cache → echo
	plugins := []Plugin{
		NewErrorsPlugin(),        // 1. 오류 처리 (가장 바깥)
		NewLogPlugin(),           // 2. 로깅
		NewCachePlugin(),         // 3. 캐싱 (조건부 중단)
		NewEchoPlugin(records),   // 4. 응답 생성 (터미널)
	}

	chain := BuildChain(plugins)

	// -------------------------------------------------------------------------
	// 3. 체인 구조 출력
	// -------------------------------------------------------------------------
	fmt.Println("플러그인 체인 구조:")
	fmt.Println("  요청 → [errors] → [log] → [cache] → [echo] → 응답")
	fmt.Println()

	// -------------------------------------------------------------------------
	// 4. 데모 시나리오들
	// -------------------------------------------------------------------------

	// 시나리오 1: 정상 쿼리 (캐시 미스 → echo 응답)
	fmt.Println("--- 시나리오 1: 첫 번째 쿼리 (캐시 미스) ---")
	response, err := chain.ServeDNS("example.com.", "A")
	printResult(response, err)

	// 시나리오 2: 동일 쿼리 반복 (캐시 히트 → echo 호출 안 함)
	fmt.Println("--- 시나리오 2: 동일 쿼리 반복 (캐시 히트) ---")
	response, err = chain.ServeDNS("example.com.", "A")
	printResult(response, err)

	// 시나리오 3: 다른 레코드 타입
	fmt.Println("--- 시나리오 3: AAAA 레코드 쿼리 ---")
	response, err = chain.ServeDNS("example.com.", "AAAA")
	printResult(response, err)

	// 시나리오 4: 존재하지 않는 레코드 (echo fallthrough → NextOrFailure → 실패)
	fmt.Println("--- 시나리오 4: 존재하지 않는 레코드 (NXDOMAIN 시뮬레이션) ---")
	response, err = chain.ServeDNS("unknown.example.com.", "A")
	printResult(response, err)

	// 시나리오 5: www 서브도메인
	fmt.Println("--- 시나리오 5: www 서브도메인 쿼리 ---")
	response, err = chain.ServeDNS("www.example.com.", "A")
	printResult(response, err)

	// -------------------------------------------------------------------------
	// 5. 체인 없는 경우 (NextOrFailure nil 핸들러)
	// -------------------------------------------------------------------------
	fmt.Println("--- 시나리오 6: 빈 체인 (nil 핸들러) ---")
	resp, nilErr := NextOrFailure("test", nil, "example.com.", "A")
	printResult(resp, nilErr)

	// -------------------------------------------------------------------------
	// 6. ClientWrite 패턴 시연
	// -------------------------------------------------------------------------
	fmt.Println("--- ClientWrite 패턴 ---")
	testCodes := []string{"NOERROR", RcodeServerFailure, "REFUSED", "NXDOMAIN"}
	for _, code := range testCodes {
		written := ClientWrite(code)
		fmt.Printf("  ClientWrite(%q) = %v\n", code, written)
	}
	fmt.Println()

	// -------------------------------------------------------------------------
	// 7. 동적 체인 재구성 (CoreDNS reload 시뮬레이션)
	// -------------------------------------------------------------------------
	fmt.Println("--- 시나리오 7: 체인 재구성 (reload) ---")
	// 캐시 없는 체인으로 재구성
	slimPlugins := []Plugin{
		NewLogPlugin(),
		NewEchoPlugin(records),
	}
	slimChain := BuildChain(slimPlugins)
	fmt.Println("재구성된 체인: [log] → [echo]")
	response, err = slimChain.ServeDNS("example.com.", "A")
	printResult(response, err)

	// -------------------------------------------------------------------------
	// 정리
	// -------------------------------------------------------------------------
	_ = errors.New("") // errors 패키지 사용 확인
	_ = strings.ToLower("") // strings 패키지 사용 확인
}

func printResult(response string, err error) {
	if err != nil {
		fmt.Printf("  결과: 오류 - %v\n", err)
	} else {
		fmt.Printf("  결과: %s\n", response)
	}
	fmt.Println()
}
