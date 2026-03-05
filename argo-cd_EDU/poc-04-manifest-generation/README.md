# PoC 04: Repo Server 매니페스트 생성

## 개요

Argo CD Repo Server는 Git 레포지토리에서 K8s 매니페스트를 생성하는 전담 컴포넌트다. 이 PoC는 소스 타입 자동 감지, 세마포어 기반 병렬성 제어, 더블체크 락킹 캐시, 에러 캐싱, `.argocd-source.yaml` 오버라이드, `ARGOCD_APP_*` 환경변수 주입, `SetAppInstance` 추적 레이블 주입 등 핵심 메커니즘을 실제 소스코드 기반으로 구현한다.

## 다루는 개념

| 개념 | 설명 | 실제 소스 |
|------|------|-----------|
| 소스 타입 감지 | Chart.yaml/kustomization.yaml 존재 여부로 자동 감지 | `reposerver/repository/repository.go:getSourceType()` |
| 세마포어 병렬 제어 | 최대 동시 매니페스트 생성 수 제한 | `reposerver/repository/repository.go:parallelismLimitSemaphore` |
| 더블체크 락킹 캐시 | 락 없는 1차 조회 → 레포 락 → 2차 확인 → 생성 | `reposerver/repository/repository.go:GenerateManifests()` |
| 에러 캐싱 | N회 실패 후 M분 동안 생성 중단 | `reposerver/repository/repository.go:PauseGenerationAfterFailedAttempts` |
| .argocd-source.yaml | 레포지토리 루트에서 App 소스 설정 오버라이드 | `reposerver/repository/repository.go:getApplicationSource()` |
| ARGOCD_APP_* 주입 | App 이름/네임스페이스/리비전을 CMP/Helm 환경변수로 전달 | `util/app/app.go:getAppEnvs()` |
| SetAppInstance | app.kubernetes.io/instance 레이블 + tracking-id 어노테이션 주입 | `util/app/app.go:SetAppInstanceLabel()` |
| Helm 생성 | helm template + values + parameters | `reposerver/repository/repository.go:helmTemplate()` |
| Kustomize 생성 | kustomize build + image overrides | `reposerver/repository/repository.go:kustomizeBuild()` |
| Directory 생성 | YAML 파일 목록 읽기 | `reposerver/repository/repository.go:findManifests()` |
| Plugin(CMP) 생성 | gRPC로 플러그인 컨테이너 호출 | `reposerver/repository/repository.go:generateManifestsFromPlugin()` |

## 매니페스트 생성 파이프라인

```
GenerateManifests(req)
│
├─ 에러 캐시 확인 (isPaused?)
│    └─ 차단 시: "생성 일시 중단" 에러 반환
│
├─ 세마포어 획득 (병렬 수 제한)
│
├─ 캐시 1차 확인 (락 없음, 빠른 조회)
│    └─ HIT: 캐시된 결과 반환
│
├─ 레포별 락 획득
│
├─ 캐시 2차 확인 (더블체크)
│    └─ HIT: 다른 goroutine이 생성한 결과 반환
│
├─ Git fetch (clone/pull)
│
├─ .argocd-source.yaml 오버라이드 적용
│
├─ 소스 타입 감지
│    ├─ Chart.yaml → Helm
│    ├─ kustomization.yaml → Kustomize
│    ├─ Plugin 설정 → CMP
│    └─ 그 외 → Directory
│
├─ ARGOCD_APP_* 환경변수 빌드
│
├─ 소스 타입별 생성
│    ├─ Helm: helm template ...
│    ├─ Kustomize: kustomize build ...
│    ├─ Directory: find *.yaml | sort
│    └─ Plugin: gRPC → CMP 컨테이너
│
├─ SetAppInstance (추적 레이블/어노테이션 주입)
│
└─ 캐시 저장
```

## 소스 타입 감지 규칙

| 조건 | 감지 결과 | 우선순위 |
|------|-----------|---------|
| `spec.source.chart` 설정됨 | Helm (차트 레포) | 1 |
| `spec.source.plugin` 설정됨 | Plugin (CMP) | 2 |
| `spec.source.helm` 설정됨 | Helm | 3 |
| `spec.source.kustomize` 설정됨 | Kustomize | 4 |
| `Chart.yaml` 파일 존재 | Helm | 5 |
| `kustomization.yaml` 파일 존재 | Kustomize | 6 |
| 위 조건 없음 | Directory | 7 (기본) |

## 추적 방식 (TrackingMethod)

| 방식 | 레이블/어노테이션 | 설명 |
|------|-----------------|------|
| `label` | `app.kubernetes.io/instance: appname` | 기본값, 하위 호환성 |
| `annotation` | `argocd.argoproj.io/tracking-id: appname:group/kind:ns/name` | 정확한 추적 |
| `annotation+label` | 둘 다 | 마이그레이션 시 사용 |

## 에러 캐싱 (ErrorCaching)

```
실패 1회 → 계속 허용
실패 2회 → 계속 허용
실패 3회 → 2분 동안 생성 중단 (에러 캐시 활성)
2분 후   → 에러 캐시 만료 → 재시도 허용
```

이 메커니즘은 Git 서버 장애 등으로 인한 레포 서버 과부하를 방지한다.

## 실행 방법

```bash
cd poc-04-manifest-generation
go run main.go
```

### 예상 출력

```
=================================================================
 Argo CD Repo Server 매니페스트 생성 시뮬레이션
=================================================================

[ 케이스 1: Helm 소스 ]
[RepoServer] 요청 수신: app=myapp, repo=..., path=helm/myapp
[RepoServer] 세마포어 획득 시도 (사용 가능: 3/3)
[RepoServer] 세마포어 획득 완료
[RepoServer] 캐시 MISS (1차): key=...
[RepoServer] Git fetch: ...
[RepoServer] .argocd-source.yaml 오버라이드 적용
[RepoServer] 소스 타입 감지: Helm
[Helm] helm template myapp-prod --namespace production -f values-prod.yaml --set image.tag=v1.2.3 ...
[RepoServer] 추적 레이블 주입: method=annotation+label, 4개 리소스

  결과: 4개 리소스, revision=a1b2c3d4e5f6, sourceType=Helm, 소요=...

[ 케이스 2: 캐시 HIT (동일 요청 재시도) ]
[RepoServer] 캐시 HIT (1차): key=...
  캐시 결과: 4개 리소스, 소요=0s (즉시 반환)
...
```

## 참조 소스코드

| 파일 | 함수 | 설명 |
|------|------|------|
| `reposerver/repository/repository.go` | `GenerateManifests()` | 메인 생성 함수, 더블체크 락킹 |
| `reposerver/repository/repository.go` | `getSourceType()` | 소스 타입 감지 |
| `reposerver/repository/repository.go` | `helmTemplate()` | Helm 매니페스트 생성 |
| `reposerver/repository/repository.go` | `kustomizeBuild()` | Kustomize 매니페스트 생성 |
| `reposerver/repository/repository.go` | `findManifests()` | Directory 매니페스트 조회 |
| `reposerver/repository/repository.go` | `getApplicationSource()` | .argocd-source.yaml 적용 |
| `util/app/app.go` | `getAppEnvs()` | ARGOCD_APP_* 환경변수 빌드 |
| `util/app/app.go` | `SetAppInstanceLabel()` | 추적 레이블 주입 |
| `reposerver/cache/cache.go` | `GetManifests()`, `SetManifests()` | 매니페스트 캐시 |

## 핵심 설계 결정

**왜 더블체크 락킹을 사용하는가?**
동일 레포지토리에 대한 다수의 동시 요청이 들어올 때, 모든 요청이 락을 기다리지 않도록 1차 캐시 조회는 락 없이 수행한다. 락 획득 후 2차 조회로 "다른 goroutine이 이미 생성했는가"를 재확인하여 중복 생성을 방지한다.

**왜 세마포어로 병렬성을 제한하는가?**
Helm template, Kustomize build, Git clone은 CPU/메모리/디스크 I/O를 상당히 소비한다. 제한 없이 병렬 실행하면 Repo Server가 과부하될 수 있으므로, 기본값 10개로 제한하여 안정성을 확보한다.

**왜 에러 캐싱이 필요한가?**
Git 서버 다운 등 지속적인 실패 상황에서 클라이언트가 계속 재시도하면 Repo Server와 Git 서버 모두 과부하된다. 실패 임계치 초과 시 일정 시간 동안 생성을 거부함으로써 시스템 전체를 보호한다.

**왜 .argocd-source.yaml이 필요한가?**
하나의 Git 레포지토리에 여러 서비스가 공존하는 모노레포 구조에서, 각 서비스가 다른 Helm values나 Kustomize 설정을 가져야 할 때 App CRD를 수정하지 않고 레포지토리 수준에서 설정을 관리할 수 있다.
