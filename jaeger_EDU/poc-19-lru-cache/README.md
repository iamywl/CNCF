# PoC-19: Jaeger LRU Cache with TTL 시뮬레이션

## 개요

Jaeger는 LRU 캐시를 TTL과 함께 사용하여 서비스/오퍼레이션 이름, sampling 확률을 캐싱한다.
이 PoC는 doubly-linked list + hashmap 기반 O(1) LRU 캐시를 구현한다.

## 실행 방법

```bash
cd jaeger_EDU/poc-19-lru-cache
go run main.go
```
