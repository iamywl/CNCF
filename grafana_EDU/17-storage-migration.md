# Grafana 스토리지와 마이그레이션 심화

## 목차

1. [개요](#1-개요)
2. [SQLStore 아키텍처](#2-sqlstore-아키텍처)
3. [다중 데이터베이스 지원](#3-다중-데이터베이스-지원)
4. [마이그레이션 시스템](#4-마이그레이션-시스템)
5. [마이그레이션 실행 순서 상세](#5-마이그레이션-실행-순서-상세)
6. [알림 마이그레이션 상세](#6-알림-마이그레이션-상세)
7. [접근 제어 마이그레이션](#7-접근-제어-마이그레이션)
8. [통합 스토리지 (Unified Storage)](#8-통합-스토리지-unified-storage)
9. [ResourceKey와 K8s 스타일 리소스 관리](#9-resourcekey와-k8s-스타일-리소스-관리)
10. [Dual-Write 전환 전략](#10-dual-write-전환-전략)
11. [Legacy에서 K8s로의 전환](#11-legacy에서-k8s로의-전환)
12. [암호화와 시크릿 관리](#12-암호화와-시크릿-관리)
13. [SecretsService 상세](#13-secretsservice-상세)
14. [데이터 키 관리와 회전](#14-데이터-키-관리와-회전)
15. [마이그레이션 가이드라인](#15-마이그레이션-가이드라인)

---

## 1. 개요

Grafana의 스토리지 레이어는 현재 대규모 아키텍처 전환을 진행 중이다.
전통적인 XORM 기반 SQL 스토리지에서 K8s(Kubernetes) 스타일의 통합 스토리지로
점진적으로 마이그레이션하고 있다.

| 구분 | Legacy | Unified (K8s) |
|------|--------|---------------|
| ORM | XORM | 없음 (직접 SQL 또는 protobuf) |
| 리소스 식별 | table + org_id + ID | namespace + group + resource + name |
| 스키마 관리 | Migrator (migrations.go) | CRD 기반 |
| 트랜잭션 | DB 세션 기반 | Event Store + ResourceVersion |
| 멀티테넌시 | org_id 컬럼 | namespace (org-{orgID}) |

### 핵심 컴포넌트 위치

| 컴포넌트 | 경로 |
|----------|------|
| SQLStore | `pkg/services/sqlstore/` |
| Migrations | `pkg/services/sqlstore/migrations/` |
| Migrator Engine | `pkg/services/sqlstore/migrator/` |
| Unified Storage | `pkg/storage/unified/` |
| Unified Resource | `pkg/storage/unified/resource/` |
| Unified API Store | `pkg/storage/unified/apistore/` |
| Unified Migrations | `pkg/storage/unified/migrations/` |
| Secrets Manager | `pkg/services/secrets/manager/` |
| Encryption | `pkg/services/encryption/` |

---

## 2. SQLStore 아키텍처

### XORM 기반 DB 추상화

Grafana의 레거시 스토리지는 XORM(Go ORM 라이브러리)을 기반으로 구축되었다.
SQLStore 서비스는 데이터베이스 연결을 관리하고, 세션과 트랜잭션을 추상화한다.

```
┌──────────────────────────────────────────────────────────────┐
│                       SQLStore                                │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │                   db.DB Interface                       │  │
│  │                                                        │  │
│  │  GetSqlxSession() → *sqlx.Session                      │  │
│  │  WithDbSession(ctx, fn) → error                        │  │
│  │  WithNewDbSession(ctx, fn) → error                     │  │
│  │  InTransaction(ctx, fn) → error                        │  │
│  │  GetDialect() → migrator.Dialect                       │  │
│  │  GetEngine() → *xorm.Engine                            │  │
│  └────────────────────────────────────────────────────────┘  │
│                          │                                    │
│            ┌─────────────┼──────────────┐                    │
│            │             │              │                    │
│            v             v              v                    │
│      ┌──────────┐ ┌──────────┐  ┌──────────────┐           │
│      │  SQLite   │ │PostgreSQL│  │    MySQL     │           │
│      │(embedded) │ │(권장)    │  │(지원)        │           │
│      └──────────┘ └──────────┘  └──────────────┘           │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 세션 관리

```go
// WithDbSession: 기존 세션 재사용 또는 새 세션 생성
func (ss *SQLStore) WithDbSession(ctx context.Context,
    callback func(sess *db.Session) error) error

// WithNewDbSession: 항상 새 세션 생성
func (ss *SQLStore) WithNewDbSession(ctx context.Context,
    callback func(sess *db.Session) error) error

// InTransaction: 트랜잭션 내에서 실행
// 콜백이 에러를 반환하면 자동 롤백
func (ss *SQLStore) InTransaction(ctx context.Context,
    fn func(ctx context.Context) error) error
```

### Dialect 패턴

데이터베이스별 SQL 방언 차이를 추상화하는 Dialect 인터페이스:

```go
type Dialect interface {
    DriverName() string
    QuoteStr() string
    AutoIncrStr() string
    BooleanStr(bool) string
    DateTimeFunc(string) string
    // ...
}
```

| 함수 | SQLite | PostgreSQL | MySQL |
|------|--------|-----------|-------|
| `QuoteStr()` | `"` | `"` | `` ` `` |
| `AutoIncrStr()` | `AUTOINCREMENT` | `SERIAL` | `AUTO_INCREMENT` |
| `BooleanStr(true)` | `1` | `true` | `1` |
| `DateTimeFunc("col")` | `datetime(col)` | `col` | `col` |

---

## 3. 다중 데이터베이스 지원

### SQLite (기본, 내장)

```ini
[database]
type = sqlite3
path = grafana.db
# 또는
# path = /var/lib/grafana/grafana.db
```

- 기본 설정으로 별도의 DB 서버 없이 사용 가능
- 단일 인스턴스 환경에 적합
- WAL 모드 활성화로 동시 읽기 성능 향상
- 제한: 동시 쓰기 제한, 멀티 인스턴스 불가

### PostgreSQL (권장)

```ini
[database]
type = postgres
host = localhost:5432
name = grafana
user = grafana
password = ${DB_PASSWORD}
ssl_mode = require
```

- 프로덕션 환경 권장
- 멀티 인스턴스(HA) 환경 지원
- 풍부한 인덱스와 쿼리 최적화
- 연결 풀링 지원

### MySQL

```ini
[database]
type = mysql
host = localhost:3306
name = grafana
user = grafana
password = ${DB_PASSWORD}
```

- MySQL 5.7+ 또는 MariaDB 10.2+ 지원
- UTF-8 인코딩 주의 필요 (utf8mb4 권장)

### 데이터베이스 선택 기준

| 기준 | SQLite | PostgreSQL | MySQL |
|------|--------|-----------|-------|
| 설치 복잡도 | 매우 낮음 | 중간 | 중간 |
| 멀티 인스턴스 | 불가 | 가능 | 가능 |
| 성능 (대규모) | 낮음 | 높음 | 중간~높음 |
| 백업 용이성 | 파일 복사 | pg_dump | mysqldump |
| 운영 비용 | 무료 | 중간 | 중간 |
| 권장 환경 | 개발/테스트 | 프로덕션 | 프로덕션 |

---

## 4. 마이그레이션 시스템

### 마이그레이션 가이드라인

`pkg/services/sqlstore/migrations/migrations.go` 파일 상단의 가이드라인:

```go
// --- Migration Guide line ---
// 1. Never change a migration that is committed and pushed to main
//    (커밋된 마이그레이션은 절대 수정하지 않는다)
// 2. Always add new migrations (to change or undo previous migrations)
//    (변경 시 항상 새로운 마이그레이션을 추가한다)
// 3. Some migrations are not yet written
//    (rename column, table, drop table, index 등)
// 4. Putting migrations behind feature flags is no longer recommended
//    (피처 플래그 뒤에 마이그레이션을 두는 것은 더 이상 권장하지 않음)
```

### OSSMigrations 구조체

```go
type OSSMigrations struct {
    features featuremgmt.FeatureToggles
}

func ProvideOSSMigrations(features featuremgmt.FeatureToggles) *OSSMigrations {
    return &OSSMigrations{features}
}
```

### Migrator 엔진

Migrator는 마이그레이션을 순차적으로 실행하고, 이미 실행된 마이그레이션은 건너뛴다.
`migration_log` 테이블에 실행 이력을 기록한다.

```
┌──────────────────────────────────────────────────────────────┐
│                        Migrator                               │
│                                                              │
│  migration_log 테이블:                                        │
│  ┌──────────┬────────────────┬──────────┬────────────────┐   │
│  │   id     │  migration_id  │  sql     │  timestamp     │   │
│  ├──────────┼────────────────┼──────────┼────────────────┤   │
│  │   1      │ create user ..│ CREATE.. │ 2024-01-01     │   │
│  │   2      │ add org table │ CREATE.. │ 2024-01-01     │   │
│  │   ...    │ ...            │ ...      │ ...            │   │
│  └──────────┴────────────────┴──────────┴────────────────┘   │
│                                                              │
│  실행 흐름:                                                   │
│  1. migration_log에서 이미 실행된 마이그레이션 목록 조회         │
│  2. 등록된 마이그레이션 중 미실행 항목 순차 실행                 │
│  3. 각 마이그레이션 실행 후 migration_log에 기록                │
│  4. 실패 시 롤백 (가능한 경우) 또는 서버 시작 중단              │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

---

## 5. 마이그레이션 실행 순서 상세

`AddMigration()` 메서드에서 등록되는 마이그레이션의 전체 순서를 분석한다:

### Phase 1: 기본 테이블 (사용자, 조직, 대시보드)

```go
mg.AddCreateMigration()           // migration_log 테이블 생성
addUserMigrations(mg)             // user 테이블
addTempUserMigrations(mg)         // temp_user 테이블 (초대 등)
addStarMigrations(mg)             // star 테이블 (즐겨찾기)
addOrgMigrations(mg)              // org, org_user 테이블
addDashboardMigration(mg)         // dashboard 테이블
addDashboardUIDStarMigrations(mg) // dashboard UID 기반 star
addDataSourceMigration(mg)        // data_source 테이블
addApiKeyMigrations(mg)           // api_key 테이블
addDashboardSnapshotMigrations(mg)// dashboard_snapshot 테이블
addQuotaMigration(mg)             // quota 테이블
```

### Phase 2: 보조 기능 테이블

```go
addAppSettingsMigration(mg)       // plugin_setting 테이블
addSessionMigration(mg)           // session 테이블
addPlaylistMigrations(mg)         // playlist 테이블
addPreferencesMigrations(mg)      // preferences 테이블
addAlertMigrations(mg)            // alert 테이블 (레거시)
addAnnotationMig(mg)              // annotation 테이블
addTestDataMigrations(mg)         // test_data 테이블
addDashboardVersionMigration(mg)  // dashboard_version 테이블
addTeamMigrations(mg)             // team, team_member 테이블
addDashboardACLMigrations(mg)     // dashboard_acl 테이블
addTagMigration(mg)               // tag 테이블
addLoginAttemptMigrations(mg)     // login_attempt 테이블
addUserAuthMigrations(mg)         // user_auth 테이블
addServerlockMigrations(mg)       // server_lock 테이블
addUserAuthTokenMigrations(mg)    // user_auth_token 테이블
addCacheMigration(mg)             // cache_data 테이블
addShortURLMigrations(mg)         // short_url 테이블
```

### Phase 3: 통합 알림 (Unified Alerting)

```go
ualert.AddTablesMigrations(mg)    // alert_rule, alert_instance 등
addLibraryElementsMigrations(mg)  // library_element 테이블
ualert.FixEarlyMigration(mg)      // 초기 마이그레이션 수정
addSecretsMigration(mg)           // data_keys, secrets 테이블
addKVStoreMigrations(mg)          // kv_store 테이블
ualert.AddDashboardUIDPanelIDMigration(mg) // alert_rule에 대시보드 UID/패널 ID 추가
```

### Phase 4: 접근 제어 (RBAC)

```go
accesscontrol.AddMigration(mg)
accesscontrol.AddDisabledMigrator(mg)
accesscontrol.AddTeamMembershipMigrations(mg)
accesscontrol.AddDashboardPermissionsMigrator(mg)
accesscontrol.AddAlertingPermissionsMigrator(mg)
```

### Phase 5: 확장 기능

```go
addCorrelationsMigrations(mg)     // correlation 테이블
addEntityEventsTableMigration(mg) // entity_event 테이블
addPublicDashboardMigration(mg)   // public_dashboard 테이블
addDbFileStorageMigration(mg)     // file_storage 테이블
addFolderMigrations(mg)           // folder 테이블
anonservice.AddMigration(mg)      // anon_device 테이블
signingkeys.AddMigration(mg)      // signing_key 테이블
```

### Phase 6: 최신 알림 확장

```go
ualert.AddRuleNotificationSettingsColumns(mg) // notification_settings 컬럼
ualert.AddRecordingRuleColumns(mg)            // 녹화 규칙 컬럼
ualert.AddStateResolvedAtColumns(mg)          // 상태 해결 시간 컬럼
ualert.AddRuleMetadata(mg)                    // 규칙 메타데이터
ualert.AddAlertRuleUpdatedByMigration(mg)     // updated_by 컬럼
ualert.AddAlertRuleStateTable(mg)             // alert_rule_state 테이블
ualert.AddAlertRuleGuidMigration(mg)          // GUID 컬럼
ualert.AddAlertRuleKeepFiringFor(mg)          // keep_firing_for 컬럼
ualert.AddAlertRuleMissingSeriesEvalsToResolve(mg) // missing_series 컬럼
ualert.AddStateFiredAtColumn(mg)              // fired_at 컬럼
ualert.AddStateAnnotationsColumn(mg)          // annotations 컬럼
ualert.AddStateEvaluationDurationColumn(mg)   // evaluation_duration 컬럼
ualert.AddStateLastErrorColumn(mg)            // last_error 컬럼
ualert.AddStateLastResultColumn(mg)           // last_result 컬럼
ualert.AddAlertRuleFolderFullpath(mg)         // folder_fullpath 컬럼
```

---

## 6. 알림 마이그레이션 상세

### 레거시에서 통합 알림으로

Grafana의 알림 시스템은 대시보드 패널 기반 레거시 알림에서 독립적인
통합 알림(Unified Alerting, ngalert)으로 전환되었다.

```
┌──────────────────────────────────────────────────────────────┐
│                레거시 알림 → 통합 알림 마이그레이션              │
│                                                              │
│  레거시:                                                     │
│  ┌──────────────┐                                            │
│  │  alert 테이블  │ → 대시보드 패널에 종속                     │
│  │  alert_notification│ → 알림 채널                           │
│  └──────────────┘                                            │
│         │                                                    │
│         v  마이그레이션                                       │
│                                                              │
│  통합 알림:                                                   │
│  ┌──────────────────────────────────────────────────────┐    │
│  │  alert_rule        │ 독립적인 알림 규칙                │    │
│  │  alert_instance    │ 규칙 인스턴스 상태                │    │
│  │  alert_configuration│ Alertmanager 설정               │    │
│  │  alert_rule_state  │ 규칙 상태 이력                   │    │
│  └──────────────────────────────────────────────────────┘    │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 알림 마이그레이션 주요 함수

| 함수 | 설명 |
|------|------|
| `AddTablesMigrations` | alert_rule, alert_instance 등 기본 테이블 |
| `AddDashboardUIDPanelIDMigration` | 대시보드 UID와 패널 ID를 alert_rule에 추가 |
| `AddRuleNotificationSettingsColumns` | 규칙별 알림 설정 컬럼 |
| `AddRecordingRuleColumns` | 녹화 규칙 지원 컬럼 |
| `AddAlertRuleGuidMigration` | 전역 고유 ID(GUID) 추가 |
| `AddAlertRuleKeepFiringFor` | keep_firing_for 기능 지원 |
| `AddAlertRuleMissingSeriesEvalsToResolve` | 누락된 시리즈 해결 평가 횟수 |
| `AddStateFiredAtColumn` | 최초 발화 시각 기록 |
| `CollateAlertRuleGroup` | 규칙 그룹 정렬 기준 설정 |
| `DropTitleUniqueIndexMigration` | 제목 유니크 인덱스 제거 |
| `AddAlertRuleGroupIndexMigration` | 그룹별 인덱스 추가 |

---

## 7. 접근 제어 마이그레이션

접근 제어(RBAC) 마이그레이션은 역할, 권한, 스코프를 관리하는 테이블과
기존 권한을 새로운 RBAC 시스템으로 전환하는 작업을 포함한다.

```go
// Phase 1: 기본 RBAC 테이블
accesscontrol.AddMigration(mg)

// Phase 2: 권한 마이그레이션
accesscontrol.AddTeamMembershipMigrations(mg)
accesscontrol.AddDashboardPermissionsMigrator(mg)
accesscontrol.AddAlertingPermissionsMigrator(mg)

// Phase 3: 관리형 권한
accesscontrol.AddManagedPermissionsMigration(mg, ...)
accesscontrol.AddManagedFolderAlertActionsMigration(mg)
accesscontrol.AddManagedFolderLibraryPanelActionsMigration(mg)

// Phase 4: Seed 할당
accesscontrol.AddSeedAssignmentMigrations(mg)

// Phase 5: 최신 권한 확장
accesscontrol.AddManagedDashboardAnnotationActionsMigration(mg)
accesscontrol.AddAlertingScopeRemovalMigration(mg)
accesscontrol.AddManagedFolderAlertingSilencesActionsMigrator(mg)
accesscontrol.AddActionSetPermissionsMigrator(mg)
accesscontrol.AddReceiverCreateScopeMigration(mg)
accesscontrol.AddOrphanedMigrations(mg)
accesscontrol.AddDatasourceDrilldownRemovalMigration(mg)
accesscontrol.AddScopedReceiverTestingPermissions(mg)
accesscontrol.AddReceiverProtectedFieldsEditor(mg)
```

---

## 8. 통합 스토리지 (Unified Storage)

### 아키텍처 개요

통합 스토리지는 Grafana의 모든 리소스를 K8s 스타일로 관리하기 위한 새로운 스토리지 레이어다.

```
┌──────────────────────────────────────────────────────────────┐
│                    Unified Storage                            │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐     │
│  │                  API Server Layer                    │     │
│  │                                                     │     │
│  │  apistore/store.go       - K8s REST storage 구현     │     │
│  │  apistore/managed.go     - 매니저드 리소스 관리       │     │
│  │  apistore/prepare.go     - 리소스 준비/검증           │     │
│  │  apistore/secure.go      - 보안 레이어               │     │
│  │  apistore/permissions.go - 권한 검사                  │     │
│  │  apistore/stream.go      - 스트리밍 지원              │     │
│  └────────────────────┬────────────────────────────────┘     │
│                       │                                      │
│  ┌────────────────────v────────────────────────────────┐     │
│  │              Resource Layer                          │     │
│  │                                                     │     │
│  │  resource/datastore.go   - 데이터 저장소 인터페이스   │     │
│  │  resource/eventstore.go  - 이벤트 저장소              │     │
│  │  resource/keys.go        - ResourceKey 검증           │     │
│  │  resource/document.go    - 문서 모델                   │     │
│  │  resource/hooks.go       - 훅 시스템                   │     │
│  │  resource/broadcaster.go - 이벤트 브로드캐스터         │     │
│  │  resource/bulk.go        - 벌크 작업                   │     │
│  └────────────────────┬────────────────────────────────┘     │
│                       │                                      │
│  ┌────────────────────v────────────────────────────────┐     │
│  │            Backend Storage                           │     │
│  │                                                     │     │
│  │  SQL Database (PostgreSQL/MySQL/SQLite)              │     │
│  │  또는                                                │     │
│  │  Object Storage (CDK Blob)                           │     │
│  │  또는                                                │     │
│  │  Parquet (parquet/reader.go, parquet/writer.go)       │     │
│  └─────────────────────────────────────────────────────┘     │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 왜 통합 스토리지인가?

1. **K8s 생태계 호환**: K8s API Server와 동일한 패턴으로 리소스 관리
2. **Watch/List 패턴**: 실시간 리소스 변경 감시 기능
3. **ResourceVersion**: 낙관적 동시성 제어
4. **Namespace 기반 멀티테넌시**: org_id 대신 namespace 사용
5. **CRD 확장성**: 새로운 리소스 타입을 쉽게 추가 가능
6. **Dual-Write**: 점진적 마이그레이션을 위한 동시 쓰기

---

## 9. ResourceKey와 K8s 스타일 리소스 관리

### ResourceKey 구조

리소스를 고유하게 식별하는 키 구조:

```protobuf
// resourcepb.ResourceKey (protobuf 정의)
message ResourceKey {
    string namespace = 1;   // 예: "org-1", "default"
    string group     = 2;   // 예: "dashboard.grafana.app"
    string resource  = 3;   // 예: "dashboards"
    string name      = 4;   // 예: "my-dashboard-uid"
}
```

### 키 검증 함수

`pkg/storage/unified/resource/keys.go`에 정의된 검증 로직:

```go
// 요청 키 검증: 모든 필드가 유효해야 함
func verifyRequestKey(key *resourcepb.ResourceKey) *resourcepb.ErrorResult {
    if err := verifyRequestKeyNamespaceGroupResource(key); err != nil {
        return NewBadRequestError(err.Message)
    }
    if err := validation.IsValidGrafanaName(key.Name); err != nil {
        return NewBadRequestError(err[0])
    }
    return nil
}

// 컬렉션 키 검증: namespace/group/resource만 검증 (name 불필요)
func verifyRequestKeyCollection(key *resourcepb.ResourceKey) *resourcepb.ErrorResult {
    return verifyRequestKeyNamespaceGroupResource(key)
}

// namespace/group/resource 검증
func verifyRequestKeyNamespaceGroupResource(key *resourcepb.ResourceKey) *resourcepb.ErrorResult {
    if key == nil {
        return NewBadRequestError("missing resource key")
    }
    if key.Group == "" {
        return NewBadRequestError("request key is missing group")
    }
    if key.Resource == "" {
        return NewBadRequestError("request key is missing resource")
    }
    if key.Namespace != clusterScopeNamespace {
        if err := validation.IsValidNamespace(key.Namespace); err != nil {
            return NewBadRequestError(err[0])
        }
    }
    return nil
}
```

### 키 매칭

```go
func matchesQueryKey(query *resourcepb.ResourceKey, key *resourcepb.ResourceKey) bool {
    if query.Group != key.Group {
        return false
    }
    if query.Resource != key.Resource {
        return false
    }
    if query.Namespace != "" && query.Namespace != key.Namespace {
        return false
    }
    if query.Name != "" && query.Name != key.Name {
        return false
    }
    return true
}
```

---

## 10. Dual-Write 전환 전략

### Dual-Write 개념

Dual-Write는 Legacy SQL과 Unified Storage에 동시에 쓰기를 수행하여
점진적 마이그레이션을 가능하게 하는 전략이다.

```
┌──────────────────────────────────────────────────────────────┐
│                  Dual-Write 전략                              │
│                                                              │
│  Mode 0: Legacy Only                                         │
│  ┌─────────┐                                                 │
│  │  Write   │───→ Legacy SQL ✓                               │
│  └─────────┘                                                 │
│                                                              │
│  Mode 1: Dual-Write (Legacy Primary)                         │
│  ┌─────────┐     ┌───────────┐                              │
│  │  Write   │──┬→│ Legacy SQL│ ✓ (Primary)                  │
│  └─────────┘  └→│ Unified   │ ✓ (Secondary)                │
│                  └───────────┘                              │
│  ┌─────────┐                                                 │
│  │  Read    │───→ Legacy SQL                                 │
│  └─────────┘                                                 │
│                                                              │
│  Mode 2: Dual-Write (Unified Primary)                        │
│  ┌─────────┐     ┌───────────┐                              │
│  │  Write   │──┬→│ Unified   │ ✓ (Primary)                  │
│  └─────────┘  └→│ Legacy SQL│ ✓ (Secondary)                │
│                  └───────────┘                              │
│  ┌─────────┐                                                 │
│  │  Read    │───→ Unified                                    │
│  └─────────┘                                                 │
│                                                              │
│  Mode 3: Unified Only                                        │
│  ┌─────────┐                                                 │
│  │  Write   │───→ Unified ✓                                  │
│  └─────────┘                                                 │
│  ┌─────────┐                                                 │
│  │  Read    │───→ Unified                                    │
│  └─────────┘                                                 │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### dualwrite.Service 인터페이스

```go
type Service interface {
    // 특정 리소스가 Unified Storage에서 읽히는지 확인
    ReadFromUnified(ctx context.Context, gr schema.GroupResource) (bool, error)

    // 이 리소스의 듀얼 라이트를 관리해야 하는지 확인
    ShouldManage(gr schema.GroupResource) bool

    // 마이그레이션 상태 확인
    Status(ctx context.Context, gr schema.GroupResource) (MigrationStatus, error)
}
```

### 마이그레이션 상태

```go
type MigrationStatus struct {
    Migrating int64   // 마이그레이션 시작 시각 (밀리초, 0이면 미진행)
    // ...
}
```

프로비저닝 서비스에서 마이그레이션 중에는 대시보드 프로비저닝을 건너뛴다:

```go
if provider.dual != nil {
    status, _ := provider.dual.Status(context.Background(),
        dashboardV1.DashboardResourceInfo.GroupResource())
    if status.Migrating > 0 {
        provider.log.Info("dashboard migrations are running, skipping provisioning",
            "elapsed", time.Since(time.UnixMilli(status.Migrating)))
        return nil
    }
}
```

---

## 11. Legacy에서 K8s로의 전환

### Namespace 매핑

```
Legacy: org_id = 1 (암시적)
K8s:    namespace = "org-1" (명시적)

Legacy: org_id = 42
K8s:    namespace = "org-42"
```

### 리소스 매핑 예시

| Legacy | K8s |
|--------|-----|
| `dashboard` 테이블 (id=123, org_id=1) | `namespace: org-1, group: dashboard.grafana.app, resource: dashboards, name: abc-uid` |
| `data_source` 테이블 (id=5, org_id=1) | `namespace: org-1, group: datasource.grafana.app, resource: datasources, name: prometheus-main` |
| `folder` 테이블 (id=10, org_id=1) | `namespace: org-1, group: folder.grafana.app, resource: folders, name: infra-folder` |

### 통합 스토리지 마이그레이션 서비스

`pkg/storage/unified/migrations/` 디렉토리에 위치한 마이그레이션 서비스:

```
unified/migrations/
├── service.go          - 마이그레이션 서비스 진입점
├── migrator.go         - 마이그레이터 엔진
├── registry.go         - 리소스 마이그레이션 등록
├── resources.go        - 리소스별 마이그레이션 정의
├── resource_migration.go - 단일 리소스 마이그레이션
├── validator.go        - 마이그레이션 검증
├── status_reader.go    - 마이그레이션 상태 읽기
├── table_locker.go     - 테이블 잠금 관리
└── testcases/          - 테스트 케이스
    ├── folders_dashboards.go
    ├── playlists.go
    └── shorturls.go
```

---

## 12. 암호화와 시크릿 관리

### Envelope Encryption 패턴

Grafana는 **봉투 암호화(Envelope Encryption)** 패턴을 사용한다.
이 패턴에서는 데이터 암호화 키(DEK)로 실제 데이터를 암호화하고,
키 암호화 키(KEK)로 DEK를 암호화한다.

```
┌──────────────────────────────────────────────────────────────┐
│                  Envelope Encryption                          │
│                                                              │
│  ┌──────────────────────────────────────────────────┐        │
│  │              Key Encryption Key (KEK)              │        │
│  │                                                    │        │
│  │  Provider에 의해 관리됨:                            │        │
│  │  - secretKey.v1 (기본, Grafana 설정 파일의 secret_key)│      │
│  │  - AWS KMS                                         │        │
│  │  - Azure Key Vault                                 │        │
│  │  - Google Cloud KMS                                │        │
│  │  - HashiCorp Vault                                 │        │
│  └───────────────────────┬──────────────────────────┘        │
│                          │ 암호화/복호화                      │
│                          v                                    │
│  ┌──────────────────────────────────────────────────┐        │
│  │           Data Encryption Key (DEK)                │        │
│  │                                                    │        │
│  │  - 랜덤 생성된 대칭키                               │        │
│  │  - data_keys 테이블에 암호화된 형태로 저장           │        │
│  │  - TTL 기반 캐싱 (기본 15분)                        │        │
│  └───────────────────────┬──────────────────────────┘        │
│                          │ 암호화/복호화                      │
│                          v                                    │
│  ┌──────────────────────────────────────────────────┐        │
│  │              실제 데이터 (평문)                      │        │
│  │                                                    │        │
│  │  - SecureJsonData (데이터소스 비밀번호, API 키)      │        │
│  │  - 알림 채널 설정                                   │        │
│  │  - OAuth 토큰                                      │        │
│  └──────────────────────────────────────────────────┘        │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### SecureJsonData 흐름

```
저장 시:
  평문 → DEK로 AES-GCM 암호화 → Base64 인코딩 → DB 저장

조회 시:
  DB 조회 → Base64 디코딩 → DEK로 AES-GCM 복호화 → 평문
```

---

## 13. SecretsService 상세

### 구조체 정의

`pkg/services/secrets/manager/manager.go`:

```go
type SecretsService struct {
    tracer     tracing.Tracer
    store      secrets.Store        // data_keys 테이블 접근
    enc        encryption.Internal  // AES-GCM 암호화 엔진
    cfg        *setting.Cfg
    features   featuremgmt.FeatureToggles
    usageStats usagestats.Service

    mtx          sync.Mutex
    dataKeyCache *dataKeyCache       // DEK 메모리 캐시

    pOnce               sync.Once
    providers           map[secrets.ProviderID]secrets.Provider  // KEK 제공자
    kmsProvidersService kmsproviders.Service

    currentProviderID secrets.ProviderID  // 현재 활성 KEK 제공자

    log log.Logger
}
```

### 초기화 흐름

```go
func ProvideSecretsService(
    tracer tracing.Tracer,
    store secrets.Store,
    kmsProvidersService kmsproviders.Service,
    enc encryption.Internal,
    cfg *setting.Cfg,
    features featuremgmt.FeatureToggles,
    usageStats usagestats.Service,
) (*SecretsService, error) {
    // 캐시 TTL 설정 (기본 15분)
    ttl := cfg.SectionWithEnvOverrides("security.encryption").
        Key("data_keys_cache_ttl").MustDuration(15 * time.Minute)

    // 현재 암호화 제공자 확인
    currentProviderID := kmsproviders.NormalizeProviderID(secrets.ProviderID(
        cfg.SectionWithEnvOverrides("security").
            Key("encryption_provider").MustString(kmsproviders.Default),
    ))

    s := &SecretsService{
        // ...
        dataKeyCache:    newDataKeyCache(ttl),
        currentProviderID: currentProviderID,
    }

    // 제공자 초기화
    err := s.InitProviders()
    if err != nil {
        return nil, err
    }

    // 현재 제공자 존재 확인
    if _, ok := s.providers[currentProviderID]; !ok {
        return nil, fmt.Errorf("missing configuration for current encryption provider %s",
            currentProviderID)
    }

    s.log.Info("Envelope encryption state", "current provider", currentProviderID)
    return s, nil
}
```

### 키 식별자 구조

암호화된 데이터에는 어떤 DEK로 암호화되었는지를 나타내는 키 식별자가 포함된다:

```go
const keyIdDelimiter = '#'

// 암호화된 데이터 형식:
// {encrypted_dek_id}#{base64_encrypted_data}
// 예: "secretKey.v1#aGVsbG8gd29ybGQ="
```

---

## 14. 데이터 키 관리와 회전

### dataKeyCache

DEK를 매번 DB에서 조회하면 성능이 저하되므로 메모리 캐시를 사용한다:

```go
type dataKeyCache struct {
    cacheTTL   time.Duration
    // dataKey ID → dataKey 매핑
    byID       map[string]dataKeyCacheItem
    // 가장 최근에 생성된 활성 키
    byLabel    map[string]dataKeyCacheItem
    mtx        sync.Mutex
}

type dataKeyCacheItem struct {
    dataKey   secrets.DataKey
    expiresAt time.Time
}
```

### 키 회전 (Key Rotation)

키 회전은 보안을 강화하기 위해 주기적으로 DEK를 교체하는 프로세스다:

```
┌──────────────────────────────────────────────────────────────┐
│                    키 회전 프로세스                            │
│                                                              │
│  1. 새 DEK 생성 (랜덤 바이트)                                 │
│  2. 새 DEK를 현재 KEK로 암호화                                │
│  3. 암호화된 DEK를 data_keys 테이블에 저장                    │
│  4. 새 DEK를 활성 키로 설정                                   │
│  5. 이후 새로운 암호화는 새 DEK 사용                          │
│  6. 기존 데이터는 여전히 이전 DEK로 복호화 가능                │
│                                                              │
│  Re-encryption (선택적):                                     │
│  7. 모든 기존 암호화된 데이터를 새 DEK로 재암호화             │
│  8. 이전 DEK 삭제                                            │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 암호화 제공자 (KMS Provider)

| 제공자 | ProviderID | 설명 |
|--------|-----------|------|
| 기본 (Grafana) | `secretKey.v1` | grafana.ini의 `secret_key` 사용 |
| AWS KMS | `awskms.{key-id}` | AWS Key Management Service |
| Azure Key Vault | `azurekeyvault.{vault}` | Azure Key Vault |
| Google Cloud KMS | `gcpkms.{key-resource}` | Google Cloud KMS |
| HashiCorp Vault | `hashicorpvault.{key}` | HashiCorp Vault Transit |

### 설정 예시

```ini
[security]
# 기본 시크릿 키 (모든 설치에서 변경 필수)
secret_key = SW2YcwTIb9zpOOhoPsMm

# 암호화 제공자 (기본: secretKey.v1)
encryption_provider = secretKey.v1

[security.encryption]
# DEK 캐시 TTL (기본 15분)
data_keys_cache_ttl = 15m

# 데이터 키 캐시 정리 간격
data_keys_cache_cleanup_interval = 1m
```

---

## 15. 마이그레이션 가이드라인

### 새 마이그레이션 추가 시 주의사항

1. **기존 마이그레이션 수정 금지**: 이미 커밋된 마이그레이션은 절대 변경하지 않는다
2. **새 마이그레이션으로 변경**: 이전 마이그레이션을 되돌리거나 수정하려면 새 마이그레이션을 추가
3. **멱등성 보장**: 같은 마이그레이션을 여러 번 실행해도 동일한 결과
4. **피처 플래그 사용 자제**: 통합 테스트에서 누락될 수 있음

### 마이그레이션 ID 규칙

```go
// 마이그레이션 ID는 고유해야 하며, 일반적으로 동작을 설명하는 문자열
mg.AddMigration("add column updated_by to alert_rule", NewAddColumnMigration(
    table, &Column{Name: "updated_by", Type: DB_NVarchar, Length: 190},
))
```

### 데이터베이스별 호환성

마이그레이션 작성 시 세 가지 데이터베이스에서 모두 동작해야 한다:

```go
// 좋은 예: Migrator가 제공하는 추상화 사용
mg.AddMigration("create user table", NewAddTableMigration(userTable))
mg.AddMigration("add column email", NewAddColumnMigration(userTable, emailCol))

// 나쁜 예: 특정 DB SQL 직접 사용 (피해야 함)
mg.AddMigration("raw sql", NewRawSQLMigration("ALTER TABLE user ADD COLUMN email VARCHAR(255)"))
```

### 대규모 데이터 마이그레이션

대량의 데이터를 변환하는 마이그레이션은 서버 시작 시간에 영향을 줄 수 있다.
이런 경우 별도의 백그라운드 마이그레이션으로 분리하거나, 배치 처리를 고려해야 한다.

```
┌──────────────────────────────────────────────────────────────┐
│             대규모 마이그레이션 전략                           │
│                                                              │
│  Option 1: 서버 시작 시 실행 (소규모)                         │
│  - 수천 건 이하의 데이터                                      │
│  - migration_log에 기록                                      │
│                                                              │
│  Option 2: 백그라운드 마이그레이션 (대규모)                    │
│  - MigrationServiceMigration 패턴 사용                       │
│  - kv_store에 진행 상태 기록                                  │
│  - 서버가 정상 실행 중에 백그라운드로 처리                     │
│                                                              │
│  Option 3: 통합 스토리지 마이그레이션 (신규)                   │
│  - unified/migrations/ 사용                                  │
│  - 리소스별 마이그레이션 등록                                  │
│  - Dual-Write와 함께 점진적 전환                              │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

---

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/services/sqlstore/migrations/migrations.go` | OSSMigrations, 전체 마이그레이션 순서 |
| `pkg/services/sqlstore/migrator/` | Migrator 엔진, Dialect, Column, Table 정의 |
| `pkg/services/sqlstore/migrations/ualert/` | 통합 알림 마이그레이션 |
| `pkg/services/sqlstore/migrations/accesscontrol/` | RBAC 마이그레이션 |
| `pkg/storage/unified/resource/keys.go` | ResourceKey 검증 |
| `pkg/storage/unified/resource/datastore.go` | 통합 스토리지 데이터스토어 인터페이스 |
| `pkg/storage/unified/resource/eventstore.go` | 이벤트 스토어 |
| `pkg/storage/unified/apistore/store.go` | K8s API 스토어 구현 |
| `pkg/storage/unified/migrations/service.go` | 통합 스토리지 마이그레이션 서비스 |
| `pkg/storage/unified/migrations/migrator.go` | 통합 스토리지 마이그레이터 |
| `pkg/storage/unified/migrations/resources.go` | 리소스별 마이그레이션 정의 |
| `pkg/storage/legacysql/dualwrite/` | Dual-Write 서비스 |
| `pkg/services/secrets/manager/manager.go` | SecretsService 구현 |
| `pkg/services/secrets/` | 시크릿 인터페이스 정의 |
| `pkg/services/encryption/` | AES-GCM 암호화 엔진 |
| `pkg/services/kmsproviders/` | KMS 제공자 팩토리 |
