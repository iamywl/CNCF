# PoC 11: Corefile 파서 (Corefile Parser)

## 개요

CoreDNS의 설정 파일인 Corefile의 파싱 메커니즘을 시뮬레이션한다. Corefile은 Caddy 서버의 Caddyfile 문법을 기반으로 하며, `zone:port { plugin args... }` 형식의 블록 구조를 사용한다.

## CoreDNS Corefile 문법

CoreDNS는 내부적으로 Caddy의 `caddyfile` 패키지를 사용하여 Corefile을 토큰화하고 파싱한다. 파싱 결과는 `ServerBlock` 단위로 구성되며, 각 블록은 zone/포트 바인딩과 플러그인 설정을 포함한다.

### 문법 구조

```
# 주석
zone1 zone2:port {
    plugin1 arg1 arg2
    plugin2 arg1 {
        sub_directive1 value1
        sub_directive2 value2
    }
}
```

### 파싱 파이프라인

```
Corefile 텍스트
  → 렉서(토큰화): 주석 제거, 따옴표 처리, 중괄호/단어 분리
  → 파서(구문 분석): zone:port 추출, 플러그인 블록 파싱
  → CorefileConfig: ServerBlock[] → PluginConfig[]
```

## 시뮬레이션 내용

1. **토큰화**: 주석(#), 따옴표 문자열, 중괄호, 줄바꿈, 일반 단어
2. **블록 파싱**: zone:port 추출 및 정규화
3. **플러그인 설정 파싱**: 인라인 인자 + 서브 블록
4. **다중 Zone 처리**: 여러 서버 블록 파싱
5. **따옴표/주석 처리**: 특수 문자 처리
6. **오류 처리**: 괄호 누락 등 문법 오류 감지

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== CoreDNS Corefile 파서 (Corefile Parser) PoC ===

--- 1. 기본 Corefile 파싱 ---
  파싱 결과:
  서버 블록 #1:
    Zone: .
    포트: 53
    플러그인 (5개):
      - errors
      - log
      - health :8080
      - cache 30
      - forward . 8.8.8.8 8.8.4.4

--- 4. 토큰화 과정 시각화 ---
  토큰 목록:
    줄    타입            값
    ---   ---             ---
    1     WORD            example.com:53
    1     OPEN_BRACE      {
    1     NEWLINE          \n
    2     WORD            cache
    2     WORD            30
```
