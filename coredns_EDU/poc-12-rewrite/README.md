# PoC 12: Rewrite 규칙 (Rewrite Rules)

## 개요

CoreDNS의 rewrite 플러그인이 DNS 요청/응답을 재작성하는 메커니즘을 시뮬레이션한다. 쿼리 이름에 대한 다양한 매칭 방식(exact, prefix, suffix, substring, regex)과 규칙 체인(stop/continue 모드)을 구현한다.

## CoreDNS Rewrite 플러그인

`plugin/rewrite/rewrite.go`에서 `Rewrite` 구조체가 `ServeDNS()`를 통해 규칙을 순차 적용한다. 각 규칙은 `Rule` 인터페이스를 구현하며, 매칭 시 `RewriteDone`, 미매칭 시 `RewriteIgnored`를 반환한다.

### 매칭 타입 (plugin/rewrite/name.go)

| 타입 | 구현 | 설명 |
|------|------|------|
| exact | `exactNameRule` | 이름 완전 일치 |
| prefix | `prefixNameRule` | 이름 접두사 일치 (`strings.CutPrefix`) |
| suffix | `suffixNameRule` | 이름 접미사 일치 (`strings.CutSuffix`) |
| substring | `substringNameRule` | 부분 문자열 포함 (`strings.Contains`) |
| regex | `regexNameRule` | 정규식 매칭 (`{0}`, `{1}` 치환) |

### 처리 모드

| 모드 | 동작 |
|------|------|
| `stop` | 규칙 매칭 시 즉시 중단, 다음 규칙 미적용 |
| `continue` | 규칙 매칭 후에도 다음 규칙 계속 적용 |

### 응답 재작성

```
클라이언트 → [요청 재작성] → 백엔드 조회 → [응답 재작성] → 클라이언트
```

`ResponseReverter`가 응답의 이름/값을 원래 쿼리 기준으로 되돌린다.

## 시뮬레이션 내용

1. **Exact Match**: 정확한 이름 일치 재작성
2. **Prefix Match**: 접두사 기반 재작성 (예: staging- → prod-)
3. **Suffix Match**: 접미사 기반 재작성 (예: .internal.example.com → .example.com)
4. **Regex Match**: 정규식 패턴 매칭 + 캡처 그룹 치환
5. **Substring Match**: 부분 문자열 치환
6. **규칙 체인**: Continue 모드로 여러 규칙 연쇄 적용
7. **응답 재작성**: 요청 재작성의 역방향 처리
8. **Stop vs Continue**: 두 모드의 동작 차이 비교

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== CoreDNS Rewrite 규칙 (Rewrite Rules) PoC ===

--- 1. Exact Match 재작성 ---
  [재작성] old-app.example.com. → new-app.example.com. (규칙: exact ...)
  [무시됨] other.example.com. (변경 없음)

--- 6. 규칙 체인 (Continue 모드) ---
  입력: staging-api.internal.corp.
    규칙 1 적용: prefix staging- → prod- [continue]
    규칙 2 적용: suffix .internal.corp. → .corp. [continue]
    규칙 3 적용: substring .corp. → .example.com. [stop]
    최종: prod-api.example.com.
```
