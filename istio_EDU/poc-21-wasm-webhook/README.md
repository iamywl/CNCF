# PoC-21: Wasm 확장 + 웹훅 관리 시뮬레이션

## 관련 문서
- [19-wasm-webhook.md](../../istio_EDU/19-wasm-webhook.md)

## 시뮬레이션 내용
1. **LocalFileCache**: OCI/HTTP URL에서 Wasm 바이너리 다운로드 → 로컬 캐시
2. **SHA256 검증**: 체크섬 불일치 시 거부
3. **TTL 기반 만료**: 캐시 엔트리 만료 후 재다운로드
4. **LRU Eviction**: 캐시 용량 초과 시 가장 오래된 엔트리 제거
5. **동시 다운로드 중복 방지**: inflight map으로 같은 URL 동시 다운로드 방지
6. **Remote → Local 변환**: ECDS 원격 참조를 로컬 파일 참조로 변환
7. **WebhookCertPatcher**: CA Bundle 자동 패칭 + 인증서 로테이션 지원

## 참조 소스
- `pkg/wasm/cache.go`
- `pkg/wasm/convert.go`
- `pkg/webhooks/webhookpatch.go`

## 실행
```bash
go run main.go
```
