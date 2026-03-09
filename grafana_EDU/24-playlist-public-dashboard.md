# 24. 재생목록 & 공개 대시보드 Deep Dive

> Grafana 소스 기준: `pkg/services/playlist/`, `pkg/services/publicdashboards/`
> 작성일: 2026-03-08

---

## 1. 개요

### 1.1 재생목록 (Playlist)

재생목록은 **여러 대시보드를 순서대로 자동 전환**하여 보여주는 기능이다. NOC(Network Operations Center)나 모니터링 TV 화면에서 주로 사용된다.

### 1.2 공개 대시보드 (Public Dashboard)

공개 대시보드는 인증 없이 **AccessToken 기반으로 대시보드를 외부에 공개**하는 기능이다. 스냅샷과 달리 **실시간 데이터**를 보여준다.

| 비교 | 재생목록 | 공개 대시보드 |
|------|---------|-------------|
| 목적 | 다중 대시보드 순환 | 단일 대시보드 외부 공개 |
| 인증 | 필요 | AccessToken만 있으면 접근 |
| 데이터 | 실시간 쿼리 | 실시간 쿼리 |
| 구성 | 대시보드 목록 + 간격 | 대시보드 1:1 매핑 |
| 생명주기 | 수동 생성/삭제 | 대시보드에 종속 |
| 제한 | 조직당 1000개 | 대시보드당 1개 |

---

## Part A: 재생목록 (Playlist)

## 2. 아키텍처

```
pkg/services/playlist/
├── playlist.go              # Service 인터페이스
├── model.go                 # 데이터 모델 (Playlist, PlaylistItem, Commands)
├── playlistimpl/
│   ├── playlist.go          # Service 구현
│   ├── xorm_store.go        # SQL Store 구현
│   └── store.go             # Store 인터페이스
└── playlisttest/
    └── fake.go              # 테스트용 Fake
```

### 왜 이런 구조인가?

Grafana의 서비스 패턴을 따른다:
1. **인터페이스 분리**: `playlist.go`에 Service 인터페이스, `store.go`에 Store 인터페이스
2. **구현 격리**: `playlistimpl/` 패키지에 실제 구현
3. **테스트 지원**: `playlisttest/fake.go`로 다른 서비스의 단위 테스트에서 사용

### Service 인터페이스

```go
// 소스: pkg/services/playlist/playlist.go
type Service interface {
    Create(context.Context, *CreatePlaylistCommand) (*Playlist, error)
    Update(context.Context, *UpdatePlaylistCommand) (*PlaylistDTO, error)
    GetWithoutItems(context.Context, *GetPlaylistByUidQuery) (*Playlist, error)
    Get(context.Context, *GetPlaylistByUidQuery) (*PlaylistDTO, error)
    Search(context.Context, *GetPlaylistsQuery) (Playlists, error)
    Delete(ctx context.Context, cmd *DeletePlaylistCommand) error
    List(ctx context.Context, orgId int64) ([]PlaylistDTO, error)
}
```

**왜 Get과 GetWithoutItems가 분리되어 있는가?** 재생목록 목록 화면에서는 아이템이 필요 없고, 상세 화면에서만 아이템이 필요하다. 불필요한 JOIN을 피해 성능을 최적화한다.

### Store 인터페이스

```go
// 소스: pkg/services/playlist/playlistimpl/store.go
type store interface {
    Insert(ctx context.Context, cmd *playlist.CreatePlaylistCommand) (*playlist.Playlist, error)
    Update(ctx context.Context, cmd *playlist.UpdatePlaylistCommand) (*playlist.PlaylistDTO, error)
    Get(ctx context.Context, query *playlist.GetPlaylistByUidQuery) (*playlist.Playlist, error)
    GetItems(ctx context.Context, query *playlist.GetPlaylistItemsByUidQuery) ([]playlist.PlaylistItem, error)
    Delete(ctx context.Context, cmd *playlist.DeletePlaylistCommand) error
    List(ctx context.Context, query *playlist.GetPlaylistsQuery) (playlist.Playlists, error)
    ListAll(ctx context.Context, orgId int64) ([]playlist.PlaylistDTO, error)
}
```

## 3. 데이터 모델

```
┌─────────────────────────────────┐     ┌──────────────────────────┐
│          Playlist               │     │     PlaylistItem         │
├─────────────────────────────────┤     ├──────────────────────────┤
│ Id       int64  (PK)            │──┐  │ Id          int64 (PK)   │
│ UID      string (unique)        │  │  │ PlaylistId  int64 (FK)   │
│ Name     string                 │  └─▶│ Type        string       │
│ Interval string ("5m", "30s")   │     │ Value       string       │
│ OrgId    int64                  │     │ Order       int          │
│ CreatedAt int64 (milliseconds)  │     │ Title       string       │
│ UpdatedAt int64 (milliseconds)  │     └──────────────────────────┘
└─────────────────────────────────┘
```

### PlaylistItem.Type 종류

| Type | Value | 설명 |
|------|-------|------|
| `dashboard_by_uid` | 대시보드 UID | 특정 대시보드 지정 |
| `dashboard_by_tag` | 태그 문자열 | 태그 기반 동적 대시보드 |
| `dashboard_by_id` | ID 숫자 | (deprecated) 내부 ID 기반 |

### DTO 계층

```
┌─────────────────────────────────────────────┐
│               PlaylistDTO                    │
├─────────────────────────────────────────────┤
│ Playlist 헤더 정보 (Id, UID, Name, Interval) │
│ + Items []PlaylistItemDTO                   │
│   ├── Type, Value                           │
│   └── Title (대시보드 제목, 프런트엔드 표시용)  │
└─────────────────────────────────────────────┘
```

**왜 DTO를 분리하는가?** DB 모델과 API 응답 모델을 분리하면:
- DB 스키마 변경이 API에 영향을 주지 않는다
- API에 필요한 계산 필드(예: Title)를 추가할 수 있다
- 불필요한 필드를 숨길 수 있다

## 4. 핵심 흐름

### 4.1 재생목록 생성

```go
// 소스: pkg/services/playlist/playlistimpl/xorm_store.go (22-73행)
func (s *sqlStore) Insert(ctx context.Context, cmd *playlist.CreatePlaylistCommand) (*playlist.Playlist, error) {
    // 1. UID 자동 생성 또는 검증
    if cmd.UID == "" {
        cmd.UID = util.GenerateShortUID()
    }

    err := s.db.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
        // 2. 조직당 최대 1000개 제한
        count, _ := sess.SQL("SELECT COUNT(*) FROM playlist WHERE playlist.org_id = ?", cmd.OrgId).Count()
        if count > MAX_PLAYLISTS {
            return fmt.Errorf("too many playlists exist (%d > %d)", count, MAX_PLAYLISTS)
        }

        // 3. Playlist 레코드 INSERT
        p = playlist.Playlist{
            Name: cmd.Name, Interval: cmd.Interval,
            OrgId: cmd.OrgId, UID: cmd.UID,
            CreatedAt: ts, UpdatedAt: ts,
        }
        sess.Insert(&p)

        // 4. PlaylistItem 레코드들 INSERT (순서 보존)
        for order, item := range cmd.Items {
            playlistItems = append(playlistItems, playlist.PlaylistItem{
                PlaylistId: p.Id,
                Type: item.Type, Value: item.Value,
                Order: order + 1,  // 1-based 순서
            })
        }
        sess.Insert(&playlistItems)
        return err
    })
}
```

```
생성 시퀀스:
  클라이언트 → POST /api/playlists
      │
      ├── 1. UID 생성 (util.GenerateShortUID)
      │
      ├── 2. 트랜잭션 시작
      │   ├── 조직당 개수 확인 (MAX_PLAYLISTS = 1000)
      │   ├── playlist INSERT
      │   └── playlist_item INSERT (순서 보존)
      │
      └── 3. Playlist 반환
```

### 4.2 List 최적화 -- 왜 두 번의 쿼리를 사용하는가?

```go
// 소스: pkg/services/playlist/playlistimpl/xorm_store.go (198-246행)
func (s *sqlStore) ListAll(ctx context.Context, orgId int64) ([]playlist.PlaylistDTO, error) {
    // 1차 쿼리: 모든 Playlist 조회
    playlists := []playlist.PlaylistDTO{}
    db.Select(ctx, &playlists, "SELECT * FROM playlist WHERE org_id=?...", orgId)

    // ID → 인덱스 매핑
    lookup := map[int64]int{}
    for i, v := range playlists {
        lookup[v.Id] = i
    }

    // 2차 쿼리: 모든 PlaylistItem을 JOIN으로 한 번에 조회
    rows, _ := db.Query(ctx, `SELECT playlist.id, playlist_item.type, playlist_item.value
        FROM playlist_item
        JOIN playlist ON playlist_item.playlist_id = playlist.id
        WHERE playlist.org_id = ?
        ORDER BY playlist_id asc, "order" asc`, orgId)

    // 결과를 메모리에서 매핑 (N+1 문제 해결)
    for rows.Next() {
        rows.Scan(&playlistId, &itemType, &itemValue)
        idx := lookup[playlistId]
        playlists[idx].Items = append(playlists[idx].Items, ...)
    }
}
```

**왜 이런 설계인가?**

```
N+1 문제 (BAD):                    2-Query 패턴 (GOOD):
  SELECT * FROM playlist            SELECT * FROM playlist
  WHERE org_id = 1                  WHERE org_id = 1
  → N개 결과                        → N개 결과 (1회)

  for each playlist:                SELECT ... FROM playlist_item
    SELECT * FROM playlist_item     JOIN playlist ON ...
    WHERE playlist_id = ?           WHERE playlist.org_id = 1
  → N번 추가 쿼리                   → 1회 추가 쿼리

  총 쿼리: N+1                      총 쿼리: 2
```

Kubernetes API의 list 명령은 전체 리소스를 반환해야 한다. 조직에 100개의 재생목록이 있으면 N+1 패턴은 101번의 쿼리를 실행하지만, 2-Query 패턴은 항상 2번만 실행한다.

### 4.3 업데이트 패턴 -- Delete + Re-insert

```go
// 소스: pkg/services/playlist/playlistimpl/xorm_store.go (75-128행)
func (s *sqlStore) Update(ctx context.Context, cmd *playlist.UpdatePlaylistCommand) {
    // 1. 기존 Playlist 조회
    sess.Get(&existingPlaylist)
    p.Id = existingPlaylist.Id
    p.CreatedAt = existingPlaylist.CreatedAt

    // 2. Playlist 헤더 UPDATE
    sess.Where("id=?", p.Id).Cols("name", "interval", "updated_at").Update(&p)

    // 3. 기존 Items 전체 DELETE
    sess.Exec("DELETE FROM playlist_item WHERE playlist_id = ?", p.Id)

    // 4. 새 Items 전체 INSERT
    sess.Insert(&playlistItems)
}
```

**왜 Delete + Re-insert인가?** 아이템 순서가 변경되면 부분 업데이트보다 전체 재삽입이 간단하고 정확하다. 트랜잭션으로 감싸져 있어 원자성이 보장된다.

```
업데이트 전:           업데이트 후:
  Item 1: Dashboard A    Item 1: Dashboard C  (순서 변경)
  Item 2: Dashboard B    Item 2: Dashboard A
  Item 3: Dashboard C    Item 3: Dashboard D  (새로 추가)
                         (Dashboard B 제거)

부분 업데이트 시 복잡도:
  - Dashboard B 삭제: DELETE WHERE ...
  - Dashboard D 추가: INSERT ...
  - Dashboard C, A 순서 변경: UPDATE order WHERE ...
  → 3종류의 SQL, 경우의 수 폭발

전체 재삽입:
  - DELETE FROM playlist_item WHERE playlist_id = ?
  - INSERT (C, A, D) with order 1, 2, 3
  → 항상 2종류의 SQL, 단순
```

### 4.4 검색 (Search)

```go
// 소스: pkg/services/playlist/playlistimpl/xorm_store.go
func (s *sqlStore) List(ctx context.Context, query *playlist.GetPlaylistsQuery) (playlist.Playlists, error) {
    playlists := make(playlist.Playlists, 0)
    sess := s.db.NewSession(ctx)
    // LIKE 검색: 이름에 쿼리 문자열이 포함된 재생목록
    sess.Where("org_id = ? AND name LIKE ?", query.OrgId, "%"+query.Name+"%")
    sess.Limit(query.Limit)
    err := sess.Find(&playlists)
    return playlists, err
}
```

**특징**: LIKE 검색으로 단순하지만 효과적이다. 재생목록은 조직당 1000개 제한이 있어 성능 문제가 없다.

## 5. Kubernetes API 통합

Grafana는 Kubernetes API Server 통합을 진행 중이다. Playlist는 이 통합의 **선구자 서비스** 중 하나이다.

```go
// 소스: pkg/services/playlist/playlistimpl/playlist.go
func (s *ServiceImpl) List(ctx context.Context, orgId int64) ([]playlist.PlaylistDTO, error) {
    return s.store.ListAll(ctx, orgId)
}
```

`ListAll`은 Kubernetes API의 LIST 동작에 대응하며, 모든 리소스를 한 번에 반환한다. 이것이 2-Query 최적화가 필요한 이유이기도 하다.

---

## Part B: 공개 대시보드 (Public Dashboard)

## 6. 아키텍처

```
pkg/services/publicdashboards/
├── api/
│   ├── api.go              # REST API 엔드포인트
│   ├── query.go            # 쿼리 핸들러
│   └── middleware.go       # AccessToken 미들웨어
├── models/
│   ├── models.go           # PublicDashboard, DTO
│   └── errors.go           # 에러 정의
├── database/
│   └── database.go         # SQL Store 구현
├── service/
│   └── service.go          # Service 구현
└── validation/             # UID, AccessToken 검증
```

### 계층 아키텍처

```
┌──────────────────────────────────────────┐
│  API Layer (api.go, middleware.go)        │
│  ├── 라우트 등록                          │
│  ├── 요청 파싱 및 검증                    │
│  └── 미들웨어 체인 (OrgID, AccessToken)   │
├──────────────────────────────────────────┤
│  Service Layer (service.go)              │
│  ├── 비즈니스 로직                        │
│  ├── 라이선스 확인                        │
│  └── 메트릭 기록                          │
├──────────────────────────────────────────┤
│  Store Layer (database.go)               │
│  ├── SQL CRUD                            │
│  └── AccessToken 유니크 제약              │
└──────────────────────────────────────────┘
```

## 7. 핵심 데이터 모델

```
┌──────────────────────────────────────────┐
│          PublicDashboard                 │
├──────────────────────────────────────────┤
│ Uid          string  (PK)                │
│ DashboardUid string  (대시보드 참조)      │
│ OrgId        int64                       │
│ AccessToken  string  (외부 접근 키)       │
│ IsEnabled    bool                        │
│ Share        ShareType (public/email)    │
│ TimeSelectionEnabled bool               │
│ AnnotationsEnabled   bool               │
│ TimeSettings  *TimeSettings             │
│ CreatedBy    int64                       │
│ UpdatedBy    int64                       │
│ CreatedAt    time.Time                   │
│ UpdatedAt    time.Time                   │
└──────────────────────────────────────────┘
```

### ShareType

| 타입 | 의미 | 라이선스 |
|------|------|---------|
| `public` | 누구나 AccessToken으로 접근 가능 | OSS |
| `email` | 이메일 기반 접근 제어 | Enterprise |

### AccessToken 설계

```
AccessToken: "32자리 랜덤 문자열"

왜 UID가 아닌 별도의 AccessToken인가?
  UID: 내부 식별자 (관리 API에서 사용)
  AccessToken: 외부 공유용 (URL에 노출됨)

분리 이유:
  1. 보안: UID를 외부에 노출하지 않음
  2. 회전: AccessToken만 재생성 가능 (UID 유지)
  3. 검색: AccessToken은 DB 인덱스로 빠른 조회
```

## 8. API 엔드포인트

### 공개 엔드포인트 (인증 불필요)

```go
// 소스: pkg/services/publicdashboards/api/api.go (71-75행)
api.routeRegister.Group("/api/public/dashboards/:accessToken", func(apiRoute routing.RouteRegister) {
    apiRoute.Get("/", routing.Wrap(api.ViewPublicDashboard))
    apiRoute.Get("/annotations", routing.Wrap(api.GetPublicAnnotations))
    apiRoute.Post("/panels/:panelId/query", routing.Wrap(api.QueryPublicDashboard))
}, api.Middleware.HandleApi)
```

### 관리 엔드포인트 (인증 + RBAC)

| 메서드 | 경로 | 권한 |
|--------|------|------|
| GET | `/api/dashboards/public-dashboards` | 로그인 필요 |
| GET | `/api/dashboards/uid/:uid/public-dashboards` | `dashboards:read` |
| POST | `/api/dashboards/uid/:uid/public-dashboards` | `dashboards.public:write` |
| PATCH | `/api/dashboards/uid/:uid/public-dashboards/:uid` | `dashboards.public:write` |
| DELETE | `/api/dashboards/uid/:uid/public-dashboards/:uid` | `dashboards.public:write` |

### 공개 vs 관리 엔드포인트 비교

```
공개 엔드포인트:                          관리 엔드포인트:
  /api/public/dashboards/:accessToken     /api/dashboards/uid/:uid/public-dashboards
  ├── 인증 불필요                          ├── 인증 + RBAC 필요
  ├── AccessToken 기반 라우팅              ├── DashboardUID 기반 라우팅
  ├── 미들웨어: OrgID 주입, 토큰 검증       ├── 표준 인증 미들웨어
  └── 읽기 전용 (조회, 쿼리)               └── CRUD (생성, 수정, 삭제)
```

## 9. 미들웨어 체인

```go
// 소스: pkg/services/publicdashboards/api/middleware.go

// 1. OrgID 주입: AccessToken에서 조직 ID를 찾아 컨텍스트에 설정
func SetPublicDashboardOrgIdOnContext(publicDashboardService) func(c *contextmodel.ReqContext) {
    orgId, _ := publicDashboardService.GetOrgIdByAccessToken(ctx, accessToken)
    c.OrgID = orgId
}

// 2. AccessToken을 컨텍스트에 설정
func SetPublicDashboardAccessToken(c *contextmodel.ReqContext) {
    c.PublicDashboardAccessToken = web.Params(c.Req)[":accessToken"]
}

// 3. AccessToken 존재 확인 (DB 조회)
func RequiresExistingAccessToken(publicDashboardService) func(c *contextmodel.ReqContext) {
    exists, _ := publicDashboardService.ExistsEnabledByAccessToken(ctx, accessToken)
    if !exists {
        c.JsonApiErr(404, "Public dashboard not found", nil)
    }
}
```

### 미들웨어 실행 순서

```
요청: GET /api/public/dashboards/abc123xyz
    │
    ├── 1. CountPublicDashboardRequest()
    │   └── 메트릭 카운터 증가
    │
    ├── 2. SetPublicDashboardOrgIdOnContext()
    │   ├── accessToken → DB 조회 → OrgID 획득
    │   └── c.OrgID = orgId (컨텍스트에 설정)
    │
    ├── 3. SetPublicDashboardAccessToken()
    │   └── c.PublicDashboardAccessToken = "abc123xyz"
    │
    ├── 4. RequiresExistingAccessToken()
    │   ├── DB 조회: IsEnabled && AccessToken 일치
    │   └── 없으면 404 반환
    │
    └── 5. 핸들러 실행 (ViewPublicDashboard)
```

## 10. 공개 대시보드 생성/업데이트 흐름

```
POST /api/dashboards/uid/{dashboardUid}/public-dashboards
    │
    ├── 1. dashboardUid 유효성 검증
    │
    ├── 2. 요청 바디 파싱 (PublicDashboardDTO)
    │
    ├── 3. uid, accessToken 유효성 검증
    │   ├── UID 길이 ≤ 40자
    │   └── AccessToken 형식 검증
    │
    ├── 4. SavePublicDashboardDTO 구성
    │   ├── UserId ← 세션
    │   ├── OrgID ← 세션
    │   └── DashboardUid ← URL 파라미터
    │
    └── 5. PublicDashboardService.Create()
        ├── AccessToken 자동 생성 (미지정 시)
        ├── 대시보드 존재 확인
        ├── 기존 공개 대시보드 중복 확인
        ├── PublicDashboard 레코드 DB 저장
        └── 응답 반환
```

## 11. 쿼리 처리

```go
// 소스: pkg/services/publicdashboards/api/query.go
func (api *Api) QueryPublicDashboard(c *contextmodel.ReqContext) response.Response {
    // 1. AccessToken으로 공개 대시보드 조회
    // 2. 패널 ID로 쿼리 대상 패널 식별
    // 3. 시간 범위 설정 (TimeSelectionEnabled에 따라)
    //    - true: 클라이언트가 지정한 시간 범위
    //    - false: 대시보드 기본 시간 범위
    // 4. 데이터소스에 쿼리 실행
    // 5. 결과 반환
}
```

**왜 TimeSelectionEnabled가 필요한가?** 공개 대시보드에서 시간 범위를 고정하면 외부 사용자가 과도한 범위의 쿼리를 실행하는 것을 방지할 수 있다.

## 12. 메트릭

```go
// 소스: pkg/services/publicdashboards/api/middleware.go (65-69행)
func CountPublicDashboardRequest() func(c *contextmodel.ReqContext) {
    return func(c *contextmodel.ReqContext) {
        metrics.MPublicDashboardRequestCount.Inc()
    }
}
```

## 13. 라이선스 제어 -- 이메일 공유

```go
// 소스: pkg/services/publicdashboards/api/api.go (161행)
if pd == nil || (!api.license.FeatureEnabled(FeaturePublicDashboardsEmailSharing) &&
    pd.Share == EmailShareType) {
    return response.Err(ErrPublicDashboardNotFound)
}
```

이메일 기반 공유는 엔터프라이즈 라이선스가 필요하다. OSS에서는 `public` 타입만 사용 가능하다.

```
OSS:
  Share = "public"  → AccessToken만으로 접근 가능
  Share = "email"   → 404 Not Found (라이선스 없음)

Enterprise:
  Share = "public"  → AccessToken만으로 접근 가능
  Share = "email"   → AccessToken + 이메일 인증 필요
```

## 14. 설계 비교: 스냅샷 vs 공개 대시보드

| 항목 | 스냅샷 | 공개 대시보드 |
|------|--------|-------------|
| 데이터 | 정적 (생성 시점 고정) | 동적 (실시간 쿼리) |
| 수명 | 만료 시간 설정 가능 | 수동 삭제까지 유지 |
| 인증 | deleteKey로 삭제만 가능 | AccessToken으로 전체 접근 |
| 리소스 | DB 저장 공간 | 쿼리 실행 비용 |
| 보안 | 한번 노출된 데이터 불변 | 실시간으로 민감 데이터 노출 가능 |
| 용도 | 특정 시점 상태 공유 | 지속적 모니터링 공유 |

## 15. 보안 고려사항

### AccessToken 보안

```
AccessToken은 URL에 포함되므로:
  1. 로그에 기록될 수 있음 → 웹서버 로그 관리 필요
  2. 브라우저 히스토리에 저장됨 → 만료 메커니즘 필요
  3. 네트워크 스니핑 가능 → HTTPS 필수

완화 방안:
  - IsEnabled 플래그로 즉시 비활성화 가능
  - AccessToken 재생성 가능 (PATCH API)
  - AnnotationsEnabled로 주석 노출 제어
  - TimeSelectionEnabled로 시간 범위 제한
```

---

## 16. PoC

- [poc-24-playlist/main.go](./poc-24-playlist/main.go): 재생목록 순환, 조직당 제한, Delete+Re-insert 업데이트 시뮬레이션
- [poc-25-public-dashboard/main.go](./poc-25-public-dashboard/main.go): 공개 대시보드 AccessToken 생성/검증, 미들웨어 체인 시뮬레이션

---

*검증 도구: Claude Code (Opus 4.6)*
