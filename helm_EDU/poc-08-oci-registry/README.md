# PoC-08: Helm v4 OCI 레지스트리

## 개요

Helm v4의 OCI 기반 차트 레지스트리(Push/Pull, manifest/layer, 컨텐츠 주소 지정)를 `net/http/httptest`로 시뮬레이션합니다.

## 시뮬레이션하는 패턴

| 패턴 | 실제 소스 | 설명 |
|------|----------|------|
| OCI 미디어 타입 | `pkg/registry/constants.go` | Config/ChartLayer/Prov 미디어 타입 |
| RegistryClient | `pkg/registry/client.go` | Push/Pull 메서드 |
| OCI Manifest | OCI Image Spec | schemaVersion + config + layers |
| 태그 규칙 | `client.go` | SemVer `+` → `_` 치환 |
| 컨텐츠 주소 지정 | OCI Distribution Spec | SHA256 digest 기반 blob 식별 |

## 실행 방법

```bash
go run main.go
```

## OCI 차트 구조

```
Manifest (OCI Image Manifest)
├── config:
│   mediaType: application/vnd.cncf.helm.config.v1+json
│   content: {"name":"myapp","version":"1.0.0",...}
│
└── layers[0]:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    content: <차트 아카이브 .tgz>
```

## Push/Pull 흐름

```
Push:
  Chart → config blob 생성 → PUT /v2/<repo>/blobs/uploads
                            → chart layer(gzip) 생성 → PUT /v2/<repo>/blobs/uploads
                            → manifest 생성 → PUT /v2/<repo>/manifests/<tag>

Pull:
  GET /v2/<repo>/manifests/<tag> → manifest 파싱
  GET /v2/<repo>/blobs/<config-digest> → config 확인
  GET /v2/<repo>/blobs/<layer-digest> → chart layer 다운로드 → 압축 해제
```

## 참조 URL 형식

```
oci://registry.example.com/myrepo/myapp:1.0.0
 │                │            │         │
 OCIScheme    레지스트리호스트  저장소경로    태그(버전)
```
