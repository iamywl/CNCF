# PoC-24: Jenkins Search + WebSocket 시뮬레이션

## 개요

Jenkins는 Trie 기반 검색 인덱스로 자동완성 검색을 제공하고, WebSocket으로 실시간 빌드 로그를 스트리밍한다.
이 PoC는 검색 인덱스, WebSocket pub/sub, 콘솔 로그 스트리밍을 시뮬레이션한다.

## 실행 방법

```bash
cd jenkins_EDU/poc-24-search-websocket
go run main.go
```
