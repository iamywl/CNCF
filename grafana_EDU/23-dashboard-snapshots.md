# 23. 대시보드 스냅샷 (Dashboard Snapshots) Deep Dive

> Grafana 소스 기준: `pkg/services/dashboardsnapshots/`
> 작성일: 2026-03-08

---

## 1. 개요

대시보드 스냅샷은 Grafana 대시보드의 **특정 시점 상태를 캡처**하여 인증 없이도 공유할 수 있게 하는 기능이다. 스냅샷에는 쿼리 결과 데이터가 직접 포함되므로, 원본 데이터소스에 접근할 수 없는 사람도 대시보드 상태를 볼 수 있다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| 데이터 포함 | 쿼리 결과가 스냅샷에 직접 임베드됨 |
| 인증 불필요 | 스냅샷 키만 있으면 누구나 접근 가능 |
| 암호화 저장 | 스냅샷 데이터가 `secrets.Service`로 암호화됨 |
| 만료 관리 | 지정된 기간 후 자동 삭제 |
| 외부 서버 지원 | 외부 Grafana 인스턴스에 스냅샷 저장 가능 |

### 왜 필요한가?

1. **데이터소스 접근 권한 없이** 대시보드 상태를 공유
2. **장애 상황 기록**: 특정 시점의 메트릭 상태를 캡처하여 사후 분석
3. **외부 공유**: 조직 외부 인원에게 대시보드 데이터 공유
4. **임시적 공유**: 만료 시간을 설정하여 자동 삭제

---

## 2. 아키텍처

```
┌─────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   HTTP API  │────▶│  Service Layer   │────▶│  Database Store  │
│  (handler)  │     │  (ServiceImpl)   │     │  (xorm/SQL)      │
└─────────────┘     └────────┬─────────┘     └─────────────────┘
                             │
                    ┌────────▼─────────┐
                    │  Secrets Service │
                    │  (암호화/복호화)   │
                    └──────────────────┘
```

### 계층 구조

```
pkg/services/dashboardsnapshots/
├── models.go          # 데이터 모델 (DashboardSnapshot, DTO, Command)
├── service.go         # Service 인터페이스 + HTTP 핸들러 로직
├── store.go           # Store 인터페이스
├── errors.go          # 에러 정의
├── service_mock.go    # Mock (테스트용)
├── service/
│   ├── service.go     # ServiceImpl (비즈니스 로직)
│   └── service_test.go
└── database/
    ├── database.go    # DashboardSnapshotStore (SQL 구현)
    └── database_test.go
```

---

## 3. 핵심 데이터 모델

### DashboardSnapshot

```
소스: pkg/services/dashboardsnapshots/models.go
```

```
┌──────────────────────────────────────────┐
│          DashboardSnapshot               │
├──────────────────────────────────────────┤
│ ID                int64  (PK, autoincr)  │
│ Name              string                 │
│ Key               string  (접근 키)       │
│ DeleteKey         string  (삭제 키)       │
│ OrgID             int64                  │
│ UserID            int64                  │
│ External          bool                   │
│ ExternalURL       string                 │
│ ExternalDeleteURL string                 │
│ Expires           time.Time              │
│ Created           time.Time              │
│ Updated           time.Time              │
│ Dashboard         *simplejson.Json       │
│ DashboardEncrypted []byte  (암호화됨)     │
└──────────────────────────────────────────┘
```

### 키 설계 — 왜 Key와 DeleteKey를 분리하는가?

이것은 보안 설계 패턴이다:

| 키 | 용도 | 누가 아는가 |
|----|------|-----------|
| Key | 스냅샷 조회 | 공유 받은 모든 사람 |
| DeleteKey | 스냅샷 삭제 | 생성자만 |

접근 키를 공유하더라도 삭제 키가 다르므로, 스냅샷 수신자가 임의로 삭제할 수 없다. 두 키 모두 32자 랜덤 문자열로 생성된다:

```go
// 소스: pkg/services/dashboardsnapshots/service.go (147-159행)
if cmd.Key == "" {
    key, err := util.GetRandomString(32)
    // ...
    cmd.Key = key
}
if cmd.DeleteKey == "" {
    deleteKey, err := util.GetRandomString(32)
    // ...
    cmd.DeleteKey = deleteKey
}
```

---

## 4. Service 인터페이스

```go
// 소스: pkg/services/dashboardsnapshots/service.go (25-32행)
type Service interface {
    CreateDashboardSnapshot(context.Context, *CreateDashboardSnapshotCommand) (*DashboardSnapshot, error)
    DeleteDashboardSnapshot(context.Context, *DeleteDashboardSnapshotCommand) error
    DeleteExpiredSnapshots(context.Context, *DeleteExpiredSnapshotsCommand) error
    GetDashboardSnapshot(context.Context, *GetDashboardSnapshotQuery) (*DashboardSnapshot, error)
    SearchDashboardSnapshots(context.Context, *GetDashboardSnapshotsQuery) (DashboardSnapshotsList, error)
    ValidateDashboardExists(context.Context, int64, string) error
}
```

---

## 5. 스냅샷 생성 흐름 (Critical Path)

### 5.1 로컬 스냅샷 생성

```
사용자 요청
    │
    ▼
CreateDashboardSnapshot()
    │
    ├── 1. 스냅샷 활성화 확인 (cfg.SnapshotsEnabled)
    │
    ├── 2. 사용자 컨텍스트에서 Identity 추출
    │
    ├── 3. 대시보드 존재 확인 (ValidateDashboardExists)
    │
    ├── 4. cmd.External == false → 로컬 스냅샷
    │   ├── 원본 대시보드 URL 생성 (/d/{uid})
    │   ├── prepareLocalSnapshot()
    │   │   ├── Dashboard JSON에 originalUrl 삽입
    │   │   ├── Key 미지정 시 32자 랜덤 생성
    │   │   └── DeleteKey 미지정 시 32자 랜덤 생성
    │   └── 스냅샷 URL 생성 (dashboard/snapshot/{key})
    │
    ├── 5. saveAndRespond()
    │   ├── ServiceImpl.CreateDashboardSnapshot()
    │   │   ├── Dashboard JSON → Marshal → []byte
    │   │   ├── secretsService.Encrypt() → 암호화된 바이트
    │   │   └── store.CreateDashboardSnapshot() → DB 저장
    │   └── JSON 응답 반환 (key, deleteKey, url, deleteUrl)
    │
    └── 6. 메트릭 기록 (MApiDashboardSnapshotCreate.Inc())
```

### 5.2 외부 스냅샷 생성

외부 스냅샷은 별도의 Grafana 인스턴스에 저장된다:

```go
// 소스: pkg/services/dashboardsnapshots/service.go (214-248행)
func createExternalDashboardSnapshot(cmd CreateDashboardSnapshotCommand,
    externalSnapshotUrl string) (*CreateExternalSnapshotResponse, error) {

    message := map[string]any{
        "name":      cmd.Name,
        "expires":   cmd.Expires,
        "dashboard": cmd.Dashboard,
        "key":       cmd.Key,
        "deleteKey": cmd.DeleteKey,
    }
    // POST {externalUrl}/api/snapshots 로 전송
    resp, err := client.Post(externalSnapshotUrl+"/api/snapshots",
        "application/json", bytes.NewBuffer(messageBytes))
    // ...
}
```

### 5.3 암호화 저장 — 왜 암호화하는가?

스냅샷에는 실제 쿼리 결과 데이터가 포함된다. 이 데이터는 민감한 비즈니스 메트릭을 담고 있을 수 있으므로, DB에 평문으로 저장하면 DB 접근 권한만 있어도 데이터가 노출된다:

```go
// 소스: pkg/services/dashboardsnapshots/service/service.go (36-50행)
func (s *ServiceImpl) CreateDashboardSnapshot(ctx context.Context,
    cmd *dashboardsnapshots.CreateDashboardSnapshotCommand) (*dashboardsnapshots.DashboardSnapshot, error) {

    marshalledData, err := cmd.Dashboard.MarshalJSON()
    // ...
    encryptedDashboard, err := s.secretsService.Encrypt(ctx, marshalledData,
        secrets.WithoutScope())  // 루트 레벨 데이터 키 사용
    // ...
    cmd.DashboardEncrypted = encryptedDashboard
    return s.store.CreateDashboardSnapshot(ctx, cmd)
}
```

조회 시에는 자동으로 복호화된다:

```go
// 소스: pkg/services/dashboardsnapshots/service/service.go (52-73행)
func (s *ServiceImpl) GetDashboardSnapshot(...) (*dashboardsnapshots.DashboardSnapshot, error) {
    queryResult, err := s.store.GetDashboardSnapshot(ctx, query)
    // ...
    if queryResult.DashboardEncrypted != nil {
        decryptedDashboard, err := s.secretsService.Decrypt(ctx,
            queryResult.DashboardEncrypted)
        dashboard, err := simplejson.NewJson(decryptedDashboard)
        queryResult.Dashboard = dashboard
    }
    return queryResult, err
}
```

---

## 6. 만료 및 정리

### 만료 시간 계산

```go
// 소스: pkg/services/dashboardsnapshots/database/database.go (47-54행)
func (d *DashboardSnapshotStore) CreateDashboardSnapshot(...) {
    var expires = time.Now().Add(time.Hour * 24 * 365 * 50) // 기본: 50년
    if cmd.Expires > 0 {
        expires = time.Now().Add(time.Second * time.Duration(cmd.Expires))
    }
    // ...
}
```

| Expires 값 | 의미 |
|-----------|------|
| 0 또는 미지정 | 50년 후 만료 (사실상 영구) |
| > 0 | 지정된 초 후 만료 |

### 만료된 스냅샷 정리

```go
// 소스: pkg/services/dashboardsnapshots/database/database.go (34-45행)
func (d *DashboardSnapshotStore) DeleteExpiredSnapshots(...) error {
    return d.store.WithDbSession(ctx, func(sess *db.Session) error {
        deleteExpiredSQL := "DELETE FROM dashboard_snapshot WHERE expires < ?"
        expiredResponse, err := sess.Exec(deleteExpiredSQL, time.Now())
        cmd.DeletedRows, _ = expiredResponse.RowsAffected()
        return nil
    })
}
```

---

## 7. 삭제 흐름

### DeleteWithKey (DeleteKey 기반 삭제)

```go
// 소스: pkg/services/dashboardsnapshots/service.go (259-276행)
func DeleteWithKey(ctx context.Context, key string, svc Service) error {
    // 1. DeleteKey로 스냅샷 조회
    query := &GetDashboardSnapshotQuery{DeleteKey: key}
    queryResult, err := svc.GetDashboardSnapshot(ctx, query)

    // 2. 외부 스냅샷이면 외부 서버에서도 삭제
    if queryResult.External {
        err := DeleteExternalDashboardSnapshot(queryResult.ExternalDeleteURL)
        // ...
    }

    // 3. 로컬 DB에서 삭제
    cmd := &DeleteDashboardSnapshotCommand{DeleteKey: queryResult.DeleteKey}
    return svc.DeleteDashboardSnapshot(ctx, cmd)
}
```

### 외부 스냅샷 삭제의 특이한 에러 처리

```go
// 소스: pkg/services/dashboardsnapshots/service.go (182-212행)
func DeleteExternalDashboardSnapshot(externalUrl string) error {
    resp, err := client.Get(externalUrl)
    // ...
    if resp.StatusCode == 200 {
        return nil
    }
    // 500 에러지만 "snapshot not found"이면 이미 삭제된 것으로 간주
    if resp.StatusCode == 500 {
        var respJson map[string]any
        json.NewDecoder(resp.Body).Decode(&respJson)
        if respJson["message"] == "Failed to get dashboard snapshot" {
            return nil  // 성공으로 처리
        }
    }
    return fmt.Errorf("unexpected response...", resp.StatusCode)
}
```

**왜 이런 설계인가?** 외부 스냅샷은 다른 서버에 저장되므로, 이미 삭제 스크립트나 다른 요청으로 삭제되었을 수 있다. 이 경우를 에러로 처리하면 로컬 DB의 스냅샷 레코드가 삭제되지 않아 좀비 레코드가 남는다.

---

## 8. 접근 제어

### 검색 시 역할 기반 필터링

```go
// 소스: pkg/services/dashboardsnapshots/database/database.go (112-153행)
func (d *DashboardSnapshotStore) SearchDashboardSnapshots(...) {
    // Admin: 조직의 모든 스냅샷 조회
    case query.SignedInUser.GetOrgRole() == org.RoleAdmin:
        sess.Where("org_id = ?", query.SignedInUser.GetOrgID())

    // 일반 사용자: 자신이 생성한 스냅샷만 조회
    case userID != 0:
        sess.Where("org_id = ? AND user_id = ?", query.OrgID, userID)

    // 서비스 계정 등: 빈 결과 반환
    default:
        queryResult = snapshots
        return nil
}
```

---

## 9. Wire DI 통합

```
┌─────────┐     ┌──────────┐     ┌───────────┐
│  db.DB  │────▶│  Store   │────▶│  Service  │
└─────────┘     │(database)│     │(service)  │
                └──────────┘     └─────┬─────┘
                                       │
┌──────────────┐                       │
│ secrets.Svc  │──────────────────────┘
└──────────────┘
┌──────────────┐                       │
│dashboard.Svc │──────────────────────┘
└──────────────┘
```

```go
// 소스: pkg/services/dashboardsnapshots/service/service.go (21-29행)
func ProvideService(
    store dashboardsnapshots.Store,
    secretsService secrets.Service,
    dashboardService dashboards.DashboardService,
) *ServiceImpl {
    return &ServiceImpl{
        store:            store,
        secretsService:   secretsService,
        dashboardService: dashboardService,
    }
}
```

---

## 10. 메트릭

| 메트릭 | 설명 |
|--------|------|
| `MApiDashboardSnapshotCreate` | 로컬 스냅샷 생성 수 |
| `MApiDashboardSnapshotExternal` | 외부 스냅샷 생성 수 |

---

## 11. 공개 모드 (Public Mode) 스냅샷

Grafana가 공개 모드로 실행 중일 때는 사용자/대시보드 검증 없이 스냅샷을 생성한다:

```go
// 소스: pkg/services/dashboardsnapshots/service.go (120-139행)
func CreateDashboardSnapshotPublic(c *contextmodel.ReqContext,
    cfg snapshot.SnapshotSharingOptions, cmd CreateDashboardSnapshotCommand,
    svc Service) {
    // 사용자 검증 없음, 외부 스냅샷 불가
    snapshotURL, err := prepareLocalSnapshot(&cmd, "")
    metrics.MApiDashboardSnapshotCreate.Inc()
    saveAndRespond(c, svc, cmd, snapshotURL)
}
```

---

## 12. 설계 비교: 스냅샷 vs 공개 대시보드

| 항목 | 스냅샷 | 공개 대시보드 |
|------|-------|-------------|
| 데이터 | 캡처 시점의 정적 데이터 | 실시간 쿼리 결과 |
| 접근 | Key 기반 | AccessToken 기반 |
| 만료 | 설정 가능 | 명시적 비활성화 필요 |
| 저장 | 암호화된 JSON | 대시보드 참조만 |
| 외부 서버 | 지원 | 미지원 |

---

## 13. PoC: poc-23-snapshot

스냅샷 시스템의 핵심 메커니즘을 시뮬레이션한다:
- 키 쌍 생성 (access key + delete key)
- 대시보드 데이터 암호화 저장
- 키 기반 조회/삭제
- 만료 관리

→ [poc-23-snapshot/main.go](./poc-23-snapshot/main.go) 참조

---

*검증 도구: Claude Code (Opus 4.6)*
