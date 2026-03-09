# 22. Jaeger Client Env2OTEL 마이그레이션 Deep-Dive

> Jaeger 소스코드 기반 분석 문서 (P2 심화)
> 분석 대상: `internal/jaegerclientenv2otel/`

---

## 1. 개요

### 1.1 마이그레이션의 배경

Jaeger 프로젝트는 2022년에 자체 클라이언트 라이브러리(jaeger-client-go, jaeger-client-java 등)를
공식 폐기(deprecate)하고, OpenTelemetry SDK로의 전환을 권장했다. 그러나 수많은 기존 서비스가
Jaeger 클라이언트의 환경변수(`JAEGER_*`)를 사용하여 트레이싱을 설정하고 있었다.

```
마이그레이션 타임라인:
  2017-2022: Jaeger Client 라이브러리 활성 개발
  2022: 공식 폐기 선언 → OTEL SDK 권장
  2023+: env2otel 마이그레이션 유틸리티 도입

문제:
  수천 개의 서비스가 JAEGER_* 환경변수 사용 중
  한 번에 모든 서비스의 환경변수를 변경할 수 없음
  점진적 마이그레이션이 필요
```

### 1.2 env2otel의 역할

`jaegerclientenv2otel` 패키지는 레거시 `JAEGER_*` 환경변수를 대응되는 `OTEL_*` 환경변수로
자동 변환하는 마이그레이션 유틸리티다.

```
변환 전:                          변환 후:
JAEGER_AGENT_HOST=collector       OTEL_EXPORTER_JAEGER_AGENT_HOST=collector
JAEGER_ENDPOINT=http://j:14268    OTEL_EXPORTER_JAEGER_ENDPOINT=http://j:14268
JAEGER_SERVICE_NAME=my-service    (무시 -- OTEL에 대응 없음)
```

### 1.3 소스 구조

```
internal/jaegerclientenv2otel/
├── envvars.go      # 환경변수 매핑 정의, 변환 함수
└── envvars_test.go  # 변환 테스트
```

이 패키지는 단 하나의 파일(`envvars.go`, 51줄)로 구성된 매우 작은 유틸리티이지만,
레거시 호환성과 마이그레이션 전략의 관점에서 중요한 설계 결정을 담고 있다.

---

## 2. 환경변수 매핑 테이블

### 2.1 전체 매핑

```go
// internal/jaegerclientenv2otel/envvars.go:13
var envVars = map[string]string{
    "JAEGER_SERVICE_NAME":                           "",
    "JAEGER_AGENT_HOST":                             "OTEL_EXPORTER_JAEGER_AGENT_HOST",
    "JAEGER_AGENT_PORT":                             "OTEL_EXPORTER_JAEGER_AGENT_PORT",
    "JAEGER_ENDPOINT":                               "OTEL_EXPORTER_JAEGER_ENDPOINT",
    "JAEGER_USER":                                   "OTEL_EXPORTER_JAEGER_USER",
    "JAEGER_PASSWORD":                               "OTEL_EXPORTER_JAEGER_PASSWORD",
    "JAEGER_REPORTER_LOG_SPANS":                     "",
    "JAEGER_REPORTER_MAX_QUEUE_SIZE":                "",
    "JAEGER_REPORTER_FLUSH_INTERVAL":                "",
    "JAEGER_REPORTER_ATTEMPT_RECONNECTING_DISABLED": "",
    "JAEGER_REPORTER_ATTEMPT_RECONNECT_INTERVAL":    "",
    "JAEGER_SAMPLER_TYPE":                           "",
    "JAEGER_SAMPLER_PARAM":                          "",
    "JAEGER_SAMPLER_MANAGER_HOST_PORT":              "",
    "JAEGER_SAMPLING_ENDPOINT":                      "",
    "JAEGER_SAMPLER_MAX_OPERATIONS":                 "",
    "JAEGER_SAMPLER_REFRESH_INTERVAL":               "",
    "JAEGER_TAGS":                                   "",
    "JAEGER_TRACEID_128BIT":                         "",
    "JAEGER_DISABLED":                               "",
    "JAEGER_RPC_METRICS":                            "",
}
```

### 2.2 매핑 분류

| 카테고리 | Jaeger 환경변수 | OTEL 대응 | 처리 |
|----------|---------------|-----------|------|
| **전송 설정** | `JAEGER_AGENT_HOST` | `OTEL_EXPORTER_JAEGER_AGENT_HOST` | 변환 |
| | `JAEGER_AGENT_PORT` | `OTEL_EXPORTER_JAEGER_AGENT_PORT` | 변환 |
| | `JAEGER_ENDPOINT` | `OTEL_EXPORTER_JAEGER_ENDPOINT` | 변환 |
| **인증** | `JAEGER_USER` | `OTEL_EXPORTER_JAEGER_USER` | 변환 |
| | `JAEGER_PASSWORD` | `OTEL_EXPORTER_JAEGER_PASSWORD` | 변환 |
| **서비스 식별** | `JAEGER_SERVICE_NAME` | (없음) | 무시 |
| **리포터 설정** | `JAEGER_REPORTER_LOG_SPANS` | (없음) | 무시 |
| | `JAEGER_REPORTER_MAX_QUEUE_SIZE` | (없음) | 무시 |
| | `JAEGER_REPORTER_FLUSH_INTERVAL` | (없음) | 무시 |
| | `JAEGER_REPORTER_ATTEMPT_*` | (없음) | 무시 |
| **샘플러 설정** | `JAEGER_SAMPLER_TYPE` | (없음) | 무시 |
| | `JAEGER_SAMPLER_PARAM` | (없음) | 무시 |
| | `JAEGER_SAMPLER_*` | (없음) | 무시 |
| **기타** | `JAEGER_TAGS` | (없음) | 무시 |
| | `JAEGER_TRACEID_128BIT` | (없음) | 무시 |
| | `JAEGER_DISABLED` | (없음) | 무시 |
| | `JAEGER_RPC_METRICS` | (없음) | 무시 |

### 2.3 변환 가능/불가능 분석

```
변환 가능 (5개):
  전송 관련 환경변수만 OTEL에 대응됨
  → 엔드포인트, 호스트, 포트, 사용자, 비밀번호

변환 불가능 (16개):
  Jaeger 고유 기능으로 OTEL에 직접적 대응이 없음
  → 서비스 이름, 샘플러, 리포터, 태그, 추적 비활성화 등
```

**왜 대부분의 환경변수가 변환 불가능한가?**

```
Jaeger Client의 설계:         OTEL SDK의 설계:
  일체형 (monolithic)          모듈형 (modular)
  - 샘플러 내장                - 별도 Sampler
  - 리포터 내장                - 별도 Exporter
  - 서비스 이름 = 환경변수     - Resource로 설정
  - 단일 설정 체계             - 컴포넌트별 설정

Jaeger의 "JAEGER_SAMPLER_TYPE=probabilistic"은
OTEL에서는 TracerProvider 생성 시 코드로 설정해야 함
→ 환경변수 변환으로 해결할 수 없음
```

---

## 3. 변환 함수 구현

### 3.1 MapJaegerToOtelEnvVars

```go
// internal/jaegerclientenv2otel/envvars.go:37
func MapJaegerToOtelEnvVars(logger *zap.Logger) {
    for jname, otelname := range envVars {
        val := os.Getenv(jname)
        if val == "" {
            continue  // 미설정 → 건너뜀
        }
        if otelname == "" {
            // 대응하는 OTEL 변수 없음 → 경고 로그
            logger.Sugar().Infof(
                "Ignoring deprecated Jaeger SDK env var %s, as there is no equivalent in OpenTelemetry",
                jname)
        } else {
            // OTEL 변수로 변환
            os.Setenv(otelname, val)
            logger.Sugar().Infof(
                "Replacing deprecated Jaeger SDK env var %s with OpenTelemetry env var %s",
                jname, otelname)
        }
    }
}
```

### 3.2 변환 알고리즘

```
MapJaegerToOtelEnvVars(logger) 호출 시:

for each (jname, otelname) in envVars:
  |
  +-- os.Getenv(jname) == "" ?
  |     |
  |     +-- YES → continue (미설정, 건너뜀)
  |     |
  |     +-- NO → val에 저장
  |
  +-- otelname == "" ?
  |     |
  |     +-- YES → logger.Info("Ignoring deprecated...")
  |     |          (대응 없음, 경고만 출력)
  |     |
  |     +-- NO → os.Setenv(otelname, val)
  |              logger.Info("Replacing deprecated...")
  |              (OTEL 환경변수 설정)
```

### 3.3 실행 예시

```
환경변수 상태:
  JAEGER_AGENT_HOST=collector.monitoring
  JAEGER_SAMPLER_TYPE=probabilistic
  JAEGER_ENDPOINT=http://jaeger:14268

실행 로그:
  INFO  Replacing deprecated Jaeger SDK env var JAEGER_AGENT_HOST
        with OpenTelemetry env var OTEL_EXPORTER_JAEGER_AGENT_HOST
  INFO  Ignoring deprecated Jaeger SDK env var JAEGER_SAMPLER_TYPE,
        as there is no equivalent in OpenTelemetry
  INFO  Replacing deprecated Jaeger SDK env var JAEGER_ENDPOINT
        with OpenTelemetry env var OTEL_EXPORTER_JAEGER_ENDPOINT

결과:
  OTEL_EXPORTER_JAEGER_AGENT_HOST=collector.monitoring  (새로 설정)
  OTEL_EXPORTER_JAEGER_ENDPOINT=http://jaeger:14268     (새로 설정)
  JAEGER_AGENT_HOST=collector.monitoring                  (그대로 유지)
  JAEGER_SAMPLER_TYPE=probabilistic                       (그대로 유지)
  JAEGER_ENDPOINT=http://jaeger:14268                     (그대로 유지)
```

---

## 4. 설계 결정 분석

### 4.1 왜 환경변수를 삭제하지 않는가?

```go
// 현재 코드: os.Setenv(otelname, val)  -- 설정만 함
// 하지 않는 것: os.Unsetenv(jname)     -- 삭제하지 않음
```

**이유:**

1. **부작용 최소화**: 다른 컴포넌트가 Jaeger 환경변수를 참조할 수 있다
2. **디버깅 용이**: 원본 환경변수가 유지되어 어떤 값이 사용되었는지 확인 가능
3. **롤백 가능**: 문제 발생 시 OTEL 변수만 삭제하면 원래 상태로 복원
4. **단방향 변환**: Jaeger→OTEL만 수행, 역방향은 불필요

### 4.2 왜 gosec 예외를 사용하는가?

```go
//nolint:gosec // G101 - env var names, not credentials
var envVars = map[string]string{
    "JAEGER_PASSWORD": "OTEL_EXPORTER_JAEGER_PASSWORD",
    ...
}
```

gosec의 G101 규칙은 코드에서 "password", "secret" 등의 문자열을 하드코딩된 자격 증명으로
감지한다. 여기서는 환경변수 **이름**일 뿐 실제 비밀번호가 아니므로 예외 처리한다.

### 4.3 왜 로그 레벨이 Info인가?

```go
logger.Sugar().Infof("Ignoring deprecated Jaeger SDK env var %s...")
logger.Sugar().Infof("Replacing deprecated Jaeger SDK env var %s...")
```

**Warn이 아닌 Info를 사용하는 이유:**

1. 마이그레이션은 예상된 동작이므로 경고(Warn)가 아닌 정보(Info)로 기록
2. 프로덕션 로그에서 Warn 이상만 모니터링하는 환경에서 불필요한 알람 방지
3. 디버깅 시 Info 레벨로 충분히 확인 가능
4. 마이그레이션 완료 후 환경변수를 정리하면 로그가 사라짐

### 4.4 왜 빈 문자열("")로 "대응 없음"을 표현하는가?

```go
"JAEGER_SERVICE_NAME": "",    // 빈 문자열 = 대응 없음
```

**대안적 접근:**
```go
// 방법 1: 별도의 무시 목록
var ignoredVars = []string{"JAEGER_SERVICE_NAME", ...}
var mappedVars = map[string]string{"JAEGER_AGENT_HOST": "OTEL_...", ...}

// 방법 2: 포인터 (nil = 대응 없음)
var envVars = map[string]*string{
    "JAEGER_SERVICE_NAME": nil,
    "JAEGER_AGENT_HOST":   ptr("OTEL_..."),
}
```

빈 문자열 접근이 선택된 이유:
1. 단일 맵으로 모든 정보를 관리하여 코드가 간결
2. 유효한 OTEL 변수명이 빈 문자열일 수 없으므로 모호함 없음
3. 새 매핑 추가 시 한 곳만 수정하면 됨

---

## 5. Jaeger Client vs OTEL SDK 환경변수 비교

### 5.1 전송 설정

```
Jaeger Client:                    OTEL SDK:
JAEGER_AGENT_HOST=localhost       OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
JAEGER_AGENT_PORT=6831            (OTLP는 단일 엔드포인트)
JAEGER_ENDPOINT=http://j:14268    OTEL_EXPORTER_OTLP_PROTOCOL=grpc

차이점:
  Jaeger: Agent(UDP) / Collector(HTTP) 분리
  OTEL: 단일 OTLP 프로토콜 (gRPC/HTTP)
```

### 5.2 서비스 식별

```
Jaeger Client:                    OTEL SDK:
JAEGER_SERVICE_NAME=my-service    OTEL_SERVICE_NAME=my-service
                                  (또는 OTEL_RESOURCE_ATTRIBUTES=service.name=my-service)

차이점:
  Jaeger: 전용 환경변수
  OTEL: Resource Attributes의 일부로 통합
  → 직접 변환이 불가능 (개념이 다름)
```

### 5.3 샘플링

```
Jaeger Client:                    OTEL SDK:
JAEGER_SAMPLER_TYPE=probabilistic OTEL_TRACES_SAMPLER=parentbased_traceidratio
JAEGER_SAMPLER_PARAM=0.1          OTEL_TRACES_SAMPLER_ARG=0.1

차이점:
  Jaeger: const, probabilistic, rateLimiting, remote
  OTEL: always_on, always_off, traceidratio, parentbased_*
  → 1:1 매핑이 불가능 (개념과 이름이 다름)
```

### 5.4 전체 비교 테이블

| 기능 | Jaeger 환경변수 | OTEL 환경변수 | 변환 가능 |
|------|---------------|-------------|----------|
| 에이전트 호스트 | JAEGER_AGENT_HOST | OTEL_EXPORTER_JAEGER_AGENT_HOST | O |
| 에이전트 포트 | JAEGER_AGENT_PORT | OTEL_EXPORTER_JAEGER_AGENT_PORT | O |
| 엔드포인트 | JAEGER_ENDPOINT | OTEL_EXPORTER_JAEGER_ENDPOINT | O |
| 사용자 | JAEGER_USER | OTEL_EXPORTER_JAEGER_USER | O |
| 비밀번호 | JAEGER_PASSWORD | OTEL_EXPORTER_JAEGER_PASSWORD | O |
| 서비스 이름 | JAEGER_SERVICE_NAME | OTEL_SERVICE_NAME | X (개념 차이) |
| 샘플러 종류 | JAEGER_SAMPLER_TYPE | OTEL_TRACES_SAMPLER | X (매핑 복잡) |
| 샘플러 매개변수 | JAEGER_SAMPLER_PARAM | OTEL_TRACES_SAMPLER_ARG | X (의존적) |
| 추적 비활성화 | JAEGER_DISABLED | (없음) | X |
| RPC 메트릭 | JAEGER_RPC_METRICS | (없음) | X (Jaeger 고유) |

---

## 6. 마이그레이션 전략

### 6.1 단계별 마이그레이션

```
Phase 1: env2otel 활성화 (Day 0)
  - Jaeger 서버에서 MapJaegerToOtelEnvVars 호출
  - 기존 JAEGER_* 환경변수 그대로 유지
  - OTEL_* 환경변수가 자동으로 추가됨
  - 동작 변화 없음

Phase 2: 서비스별 OTEL SDK 전환 (Week 1~N)
  - 각 서비스를 Jaeger Client → OTEL SDK로 변경
  - 환경변수를 JAEGER_* → OTEL_* 로 변경
  - 서비스별 독립적 전환 가능

Phase 3: 레거시 정리 (마이그레이션 완료 후)
  - 모든 JAEGER_* 환경변수 제거
  - env2otel 코드 비활성화 가능
  - 로그에서 "deprecated" 메시지 사라짐
```

### 6.2 마이그레이션 모니터링

```
로그 기반 마이그레이션 진행 상황 추적:

마이그레이션 전:
  grep "Replacing deprecated" logs | wc -l → 5 (변환 중인 변수)
  grep "Ignoring deprecated" logs | wc -l → 16 (무시되는 변수)

마이그레이션 완료:
  grep "deprecated" logs | wc -l → 0 (모든 JAEGER_* 제거됨)
```

### 6.3 호환성 매트릭스

```
+------------------+------------------+------------------+
| 서비스 SDK       | Jaeger 서버      | 동작             |
+------------------+------------------+------------------+
| Jaeger Client    | env2otel 활성    | 정상 (자동 변환) |
| OTEL SDK         | env2otel 활성    | 정상 (변환 불필요)|
| Jaeger Client    | env2otel 비활성  | 정상 (직접 지원) |
| OTEL SDK         | env2otel 비활성  | 정상 (OTEL 네이티브)|
+------------------+------------------+------------------+
```

---

## 7. 보안 고려사항

### 7.1 자격 증명 전파

```go
"JAEGER_USER":     "OTEL_EXPORTER_JAEGER_USER",
"JAEGER_PASSWORD":  "OTEL_EXPORTER_JAEGER_PASSWORD",
```

비밀번호가 환경변수에서 환경변수로 복사되므로, 보안 수준은 변하지 않는다.
그러나 다음 사항에 주의해야 한다:

1. **로그에 값이 노출되지 않음**: 로그는 환경변수 **이름**만 기록, 값은 기록하지 않음
2. **프로세스 환경**: `os.Setenv`로 설정된 값은 `/proc/PID/environ`에서 볼 수 있음
3. **자식 프로세스**: `os.Setenv` 이후 fork된 자식 프로세스에 OTEL 변수가 상속됨

### 7.2 환경변수 우선순위

```
OTEL_EXPORTER_JAEGER_ENDPOINT이 이미 설정된 상태에서
JAEGER_ENDPOINT도 설정된 경우:

현재 코드: os.Setenv(otelname, val)
→ OTEL 변수가 JAEGER 변수의 값으로 덮어씌워짐

이는 의도적인 동작:
  JAEGER_* 변수가 존재한다면, 이를 우선하여 마이그레이션을 수행
  OTEL_* 변수만 사용하고 싶다면 JAEGER_* 변수를 제거해야 함
```

---

## 8. 테스트 전략

### 8.1 환경변수 격리

```
테스트에서 환경변수를 조작하므로, 격리가 중요하다:

1. t.Setenv() 사용: 테스트 종료 시 자동 복원
2. 각 테스트 케이스가 독립적
3. 병렬 실행 시 환경변수 충돌 주의
```

### 8.2 테스트 시나리오

```
테스트 1: 변환 가능한 변수
  입력: JAEGER_AGENT_HOST=myhost
  기대: OTEL_EXPORTER_JAEGER_AGENT_HOST=myhost

테스트 2: 변환 불가능한 변수
  입력: JAEGER_SAMPLER_TYPE=probabilistic
  기대: Info 로그 출력, OTEL 변수 미설정

테스트 3: 미설정 변수
  입력: (JAEGER_AGENT_HOST 미설정)
  기대: 아무 동작 없음

테스트 4: 빈 값
  입력: JAEGER_AGENT_HOST=""
  기대: os.Getenv("") == "" → continue (건너뜀)
```

---

## 9. 유사 프로젝트와의 비교

### 9.1 다른 프로젝트의 마이그레이션 접근

| 프로젝트 | 마이그레이션 방식 | 특징 |
|----------|----------------|------|
| Jaeger | 환경변수 자동 변환 | 런타임에 투명하게 동작 |
| Spring Cloud Sleuth → Micrometer | 설정 파일 마이그레이션 가이드 | 수동 변환 |
| Zipkin Brave → OTEL | Bridge/Shim 라이브러리 | API 호환 레이어 |
| DataDog → OTEL | dd-trace-java otel-api 지원 | 이중 API 지원 |

### 9.2 Jaeger 접근의 장단점

```
장점:
  - 코드 변경 없이 환경변수만으로 마이그레이션
  - 점진적 전환 가능
  - 운영자 수준에서 제어 가능

단점:
  - 5개 변수만 변환 가능 (16개는 무시)
  - 서비스 이름, 샘플러 등 핵심 설정은 코드 변경 필요
  - OTEL의 Resource Attributes 개념은 환경변수만으로 완전히 전환 불가
```

---

## 10. 확장 가능성

### 10.1 커스텀 매핑 추가

env2otel 매핑은 정적 맵이므로, 새로운 환경변수 매핑을 추가하는 것은 간단하다:

```go
// 새 매핑 추가 (가상 예시)
var envVars = map[string]string{
    // 기존 매핑...
    "JAEGER_CUSTOM_HEADER": "OTEL_EXPORTER_OTLP_HEADERS",  // 새 매핑
}
```

### 10.2 양방향 변환

현재는 Jaeger→OTEL 단방향만 지원하지만, 이론적으로 역방향도 가능하다:

```go
// 역방향 변환 (현재 미구현)
func MapOtelToJaegerEnvVars(logger *zap.Logger) {
    for jname, otelname := range envVars {
        if otelname == "" {
            continue
        }
        if val := os.Getenv(otelname); val != "" {
            os.Setenv(jname, val)
        }
    }
}
```

그러나 OTEL→Jaeger 역방향 변환은 의도적으로 구현하지 않았다.
마이그레이션은 항상 레거시→새 시스템 방향으로만 진행되어야 한다.

---

## 11. 정리

### 11.1 핵심 설계 원칙

| 원칙 | 구현 |
|------|------|
| 최소 개입 | 환경변수만 추가, 삭제/수정하지 않음 |
| 투명성 | 모든 변환/무시를 Info 로그로 기록 |
| 안전성 | 자격 증명 값을 로그에 노출하지 않음 |
| 단순성 | 51줄 코드, 단일 함수 |
| 점진적 전환 | 서비스별 독립적 마이그레이션 가능 |

### 11.2 숫자로 보는 env2otel

| 항목 | 값 |
|------|-----|
| 총 코드 줄수 | 51줄 |
| 지원 Jaeger 환경변수 | 21개 |
| 변환 가능 | 5개 (24%) |
| 무시 (대응 없음) | 16개 (76%) |
| 외부 의존성 | 1개 (go.uber.org/zap) |
| 함수 수 | 1개 (MapJaegerToOtelEnvVars) |

### 11.3 관련 소스 파일 요약

| 파일 | 줄수 | 핵심 함수/타입 |
|------|------|---------------|
| `internal/jaegerclientenv2otel/envvars.go` | 51줄 | `envVars` 맵, `MapJaegerToOtelEnvVars` |

### 11.4 PoC 참조

- `poc-21-env2otel/` -- 환경변수 매핑과 마이그레이션 로직 시뮬레이션

---

## 부록: 전체 Jaeger Client 환경변수 레퍼런스

| 환경변수 | 기본값 | 설명 |
|----------|--------|------|
| `JAEGER_SERVICE_NAME` | (필수) | 서비스 이름 |
| `JAEGER_AGENT_HOST` | `localhost` | UDP 에이전트 호스트 |
| `JAEGER_AGENT_PORT` | `6831` | UDP 에이전트 포트 |
| `JAEGER_ENDPOINT` | (없음) | HTTP Collector 엔드포인트 |
| `JAEGER_USER` | (없음) | HTTP Basic Auth 사용자 |
| `JAEGER_PASSWORD` | (없음) | HTTP Basic Auth 비밀번호 |
| `JAEGER_REPORTER_LOG_SPANS` | `false` | 스팬 로깅 활성화 |
| `JAEGER_REPORTER_MAX_QUEUE_SIZE` | `100` | 리포터 큐 크기 |
| `JAEGER_REPORTER_FLUSH_INTERVAL` | `1s` | 리포터 플러시 주기 |
| `JAEGER_SAMPLER_TYPE` | `remote` | 샘플러 종류 |
| `JAEGER_SAMPLER_PARAM` | `0.001` | 샘플러 매개변수 |
| `JAEGER_SAMPLER_MANAGER_HOST_PORT` | `localhost:5778` | 원격 샘플링 서버 |
| `JAEGER_SAMPLING_ENDPOINT` | (없음) | HTTP 샘플링 엔드포인트 |
| `JAEGER_TAGS` | (없음) | 전역 태그 (key=value 쉼표 구분) |
| `JAEGER_TRACEID_128BIT` | `true` | 128비트 TraceID 사용 |
| `JAEGER_DISABLED` | `false` | 추적 비활성화 |
| `JAEGER_RPC_METRICS` | `false` | RPC 메트릭 수집 |

---

*본 문서는 Jaeger 소스코드의 `internal/jaegerclientenv2otel/envvars.go`를 직접 분석하여 작성되었다.*
