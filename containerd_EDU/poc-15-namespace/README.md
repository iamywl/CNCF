# PoC-15: containerd 네임스페이스 격리 시뮬레이션

## 목적

containerd의 네임스페이스가 Docker, Kubernetes, BuildKit 등 서로 다른 클라이언트의
리소스를 논리적으로 격리하는 방식을 시뮬레이션한다. context.Context에 네임스페이스를
주입하여 모든 API 호출의 범위를 결정하는 패턴과, BoltDB에서의 저장 구조를 이해한다.

## 핵심 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| WithNamespace | `pkg/namespaces/context.go` | context에 네임스페이스 주입 |
| NamespaceRequired | `pkg/namespaces/context.go` | 네임스페이스 없으면 에러 반환 |
| 기본 네임스페이스 | `pkg/namespaces/context.go` | Default = "default" |
| namespaceStore | `core/metadata/namespaces.go` | Create, List, Delete |
| BoltDB 경로 | `core/metadata/buckets.go` | v1/<ns>/images, containers 등 |
| 삭제 보호 | `core/metadata/namespaces.go` | 리소스 잔존 시 삭제 거부 |

## 소스 참조

| 파일 | 핵심 내용 |
|------|----------|
| `pkg/namespaces/context.go` | WithNamespace, Namespace, NamespaceRequired, CONTAINERD_NAMESPACE 환경변수 |
| `pkg/namespaces/grpc.go` | gRPC 메타데이터 헤더에 네임스페이스 설정/추출 |
| `pkg/namespaces/ttrpc.go` | tTRPC 메타데이터 헤더에 네임스페이스 설정/추출 |
| `core/metadata/namespaces.go` | namespaceStore.Create/List/Delete, listNs (잔존 리소스 확인) |
| `core/metadata/buckets.go` | BoltDB 스키마: v1/<ns>/images, containers, content, snapshots, sandboxes, leases |

## 시뮬레이션 흐름

```
1. 네임스페이스 생성: default, moby, k8s.io, buildkit
2. 같은 이미지를 다른 네임스페이스에서 독립적으로 관리
   - moby:     nginx:1.25 (digest: sha256:aaa111)
   - k8s.io:   nginx:1.25 (digest: sha256:bbb222)
   - buildkit: nginx:1.25 (digest: sha256:ccc333)
3. 네임스페이스별 이미지 조회 — 각자 자기 것만 보임
4. 네임스페이스별 컨테이너 격리
5. NamespaceRequired 에러 처리 — 네임스페이스 없으면 에러
6. 네임스페이스 삭제 — 리소스 있으면 거부
7. 리소스 정리 후 삭제 성공
8. BoltDB 스키마 시각화
```

## 실행 방법

```bash
cd containerd_EDU/poc-15-namespace
go run main.go
```

## 예상 출력

```
=== containerd 네임스페이스 격리 시뮬레이션 ===

[1] 네임스페이스 생성
  [생성] default      — 기본 네임스페이스 (ctr 명령어 등)
  [생성] moby         — Docker 엔진 전용
  [생성] k8s.io       — Kubernetes CRI 전용
  [생성] buildkit     — BuildKit 빌더 전용

[2] 같은 이미지를 다른 네임스페이스에서 독립 관리
  [moby]     이미지 추가: docker.io/library/nginx:1.25 (digest: sha256:aaa111)
  [k8s.io]   이미지 추가: docker.io/library/nginx:1.25 (digest: sha256:bbb222)
  [buildkit] 이미지 추가: docker.io/library/nginx:1.25 (digest: sha256:ccc333)

[3] 네임스페이스별 이미지 조회 (격리 확인)
  [moby    ] 이미지 수: 1
             docker.io/library/nginx:1.25 (digest: sha256:aaa111)
  [k8s.io  ] 이미지 수: 1
             docker.io/library/nginx:1.25 (digest: sha256:bbb222)
  [buildkit] 이미지 수: 1
  [default ] 이미지 수: 0

[5] NamespaceRequired 에러 처리
  네임스페이스 없는 context: namespace is required

[6] 네임스페이스 삭제 — 리소스 잔존 시 거부
  [moby] 삭제 시도: namespace "moby" must be empty, but it still has images, containers

[7] 리소스 정리 후 네임스페이스 삭제
  [moby] 네임스페이스 삭제 성공
```

## 핵심 포인트

- **논리적 멀티 테넌시**: containerd 네임스페이스는 Linux 네임스페이스와 다르다. 같은 containerd 데몬을 공유하면서도 Docker("moby"), Kubernetes("k8s.io"), BuildKit("buildkit") 등이 서로의 리소스를 볼 수 없다.
- **Context 기반 격리**: 모든 API 호출의 context에 네임스페이스가 포함되어야 한다. `NamespaceRequired(ctx)`가 실패하면 API 호출 자체가 거부된다.
- **완전한 리소스 격리**: Image, Container, Snapshot, Content, Sandbox, Lease 등 모든 리소스가 네임스페이스별로 독립적이다. 같은 이미지 이름이어도 네임스페이스가 다르면 별개의 리소스이다.
- **삭제 보호**: 네임스페이스 안에 리소스가 남아 있으면 삭제가 거부된다. `listNs()` 함수가 images, blobs, containers, snapshots 버킷을 검사한다.
- **BoltDB 계층**: `v1/<namespace>/<object>/<key>` 구조로, 네임스페이스가 BoltDB의 버킷 경로에 직접 반영된다.
