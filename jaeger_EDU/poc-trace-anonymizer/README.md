# PoC 15: 트레이스 익명화(Anonymizer) 시뮬레이션

## 개요

Jaeger의 트레이스 익명화 시스템을 시뮬레이션합니다.
실제 프로덕션 트레이스에서 민감한 정보(서비스명, 오퍼레이션명, 커스텀 태그 등)를 제거하거나 해싱하여
안전하게 외부 연구자나 커뮤니티와 공유할 수 있게 변환하는 과정을 구현합니다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `cmd/anonymizer/app/anonymizer/anonymizer.go` | Anonymizer, hash(), allowedTags, Options |
| `cmd/anonymizer/app/writer/writer.go` | Writer - 원본/익명화 스팬 JSON 출력 |

## 핵심 설계 원리

### FNV-1a 64비트 해싱
```go
func hash(value string) string {
    h := fnv.New64()
    h.Write([]byte(value))
    return fmt.Sprintf("%016x", h.Sum64())
}
```
- 빠르고 결정적 (같은 입력 = 같은 출력)
- 16바이트 16진수 문자열 출력
- 비가역 (역추적 불가, 매핑 파일이 필요한 이유)

### 허용된 표준 태그 (절대 해싱되지 않음)
```go
var allowedTags = map[string]bool{
    "error": true, "http.method": true,
    "http.status_code": true, "span.kind": true,
    "sampler.type": true, "sampler.param": true,
}
```

### 4가지 해싱 옵션
| 옵션 | false (기본) | true |
|------|-------------|------|
| HashStandardTags | 표준 태그 보존 | 표준 태그도 해싱 |
| HashCustomTags | 커스텀 태그 삭제 | 커스텀 태그 해싱 |
| HashLogs | 로그 삭제 | 로그 필드 해싱 |
| HashProcess | 프로세스 태그 삭제 | 프로세스 태그 해싱 |

### 매핑 파일
원본 → 해시 대응 관계를 JSON으로 저장하여 필요 시 역추적 가능합니다.

## 시뮬레이션 내용

1. **FNV64 해싱 기본 동작**: 다양한 문자열에 대한 해시 결과 확인
2. **허용된 표준 태그**: 보존되는 태그 목록 설명
3. **기본 익명화**: 커스텀 태그/로그/프로세스 태그 삭제
4. **전체 해싱**: 모든 옵션 활성화 시 동작
5. **매핑 파일**: 원본↔해시 대응 관계 저장
6. **일관성 검증**: 같은 입력에 대한 결정적 해싱 확인
7. **런 간 일관성**: 매핑 파일 재사용으로 다른 실행에서도 동일 결과
8. **error 태그 정규화**: 특수한 error 태그 처리 로직

## 실행 방법

```bash
go run main.go
```

## 주요 출력

- FNV64 해시 변환 테이블
- 원본/익명화 스팬 JSON 비교
- 옵션별 익명화 결과 차이
- 매핑 파일 내용
- 일관성 검증 결과 (스팬 간, 런 간)

## 핵심 인사이트

- 서비스명과 오퍼레이션명은 항상 해싱됨 (사이트 특정 정보)
- 표준 태그는 분석에 필수적이므로 기본적으로 보존
- 커스텀 태그에는 SQL문, 고객 ID 등 민감 정보가 포함될 수 있어 기본적으로 삭제
- 오퍼레이션명 매핑 키는 `[서비스]:오퍼레이션` 형식 → 서로 다른 서비스의 같은 이름 오퍼레이션도 구분
- 매핑 파일은 10초마다 자동 저장 (실제 코드의 ticker) + 종료 시 최종 저장
