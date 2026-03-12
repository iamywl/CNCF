# PoC 14: DNS 와일드카드 매칭

## 개요

CoreDNS file 플러그인의 와일드카드 DNS 레코드 처리를 시뮬레이션한다.
RFC 4592에 따른 와일드카드 우선순위, 다단계 매칭, Empty Non-Terminal 처리를 구현한다.

## 실제 CoreDNS 코드 참조

| 파일 | 역할 |
|------|------|
| `plugin/file/wildcard.go` | `replaceWithAsteriskLabel()` 함수 |
| `plugin/file/lookup.go` | 와일드카드 조회 로직 |
| `plugin/file/tree/elem.go` | 트리 기반 레코드 검색 |

## 핵심 동작

1. **정확 매칭 우선**: `www.example.com`은 `*.example.com`보다 정확 매칭이 우선
2. **와일드카드 매칭**: `*.example.com` → `unknown.example.com` 매칭
3. **다단계 와일드카드**: `*.staging.example.com` → `app.staging.example.com` 매칭
4. **Empty Non-Terminal**: 하위 레코드가 존재하면 와일드카드 매칭 차단
5. **replaceWithAsteriskLabel()**: 가장 왼쪽 레이블을 `*`로 교체하여 와일드카드 검색

## 실행

```bash
go run main.go
```

## 테스트 케이스

- 정확 매칭 vs 와일드카드 우선순위
- 와일드카드 A/TXT 레코드 매칭
- 다단계 와일드카드 (staging 서브도메인)
- 상위 와일드카드 폴백
- Empty Non-Terminal에 의한 와일드카드 차단
- NODATA (이름 존재, 타입 없음)
- NXDOMAIN (매칭 없음)
