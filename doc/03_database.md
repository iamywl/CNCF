# Database

클라우드 네이티브 환경에서 사용되는 데이터베이스. 관계형, NoSQL, 시계열, 벡터 등 다양한 유형을 포함.

---

## Graduated

### TiKV ★16.5k
- **역할**: 분산 트랜잭셔널 KV(Key-Value) 스토어
- **설계**: Google Spanner + HBase 영감, Raft 합의 알고리즘
- **왜 쓰나**: 강력한 일관성(strong consistency)을 보장하는 분산 저장소. TiDB의 스토리지 엔진
- **비유**: 여러 서버에 분산된 초고속 사전 — 어디서 읽어도 같은 답

### Vitess ★20.8k
- **역할**: MySQL 수평 확장 미들웨어
- **출발**: YouTube에서 MySQL 확장 문제 해결을 위해 개발
- **왜 쓰나**: 기존 MySQL을 투명하게 샤딩(sharding) — 앱 코드 변경 없이 수평 확장
- **비유**: MySQL 앞에 놓는 교통 정리 경찰 — 쿼리를 올바른 샤드로 라우팅

---

## Sandbox

| 도구 | ★ | 설명 |
|---|---|---|
| **CloudNativePG** | 8.1k | K8s에서 PostgreSQL 운영 Operator (전체 라이프사이클 관리) |
| **openGemini** | 1.1k | 분산 시계열 DB (관측 데이터 특화, 고동시성) |
| **SchemaHero** | 1.1k | K8s에서 DB 스키마를 GitOps로 관리하는 Operator |

---

## 주요 비-CNCF 도구

### 관계형 데이터베이스 (RDBMS)

| 도구 | ★ | 설명 |
|---|---|---|
| **PostgreSQL** | 20.2k | 가장 인기 있는 오픈소스 RDBMS. 확장성과 표준 준수 우수 |
| **MySQL** | 12.2k | 세계에서 가장 많이 쓰이는 오픈소스 DB (Oracle 소유) |
| **MariaDB** | 7.2k | MySQL 포크. MySQL 원 개발자가 만든 커뮤니티 버전 |
| **Supabase** | 98.3k | 오픈소스 Firebase 대안 (PostgreSQL + 인증 + 실시간 + 스토리지) |

### NewSQL (분산 SQL)

| 도구 | ★ | 설명 |
|---|---|---|
| **CockroachDB** | 32k | 분산 SQL. 지리적 분산 + 강한 일관성 (Google Spanner 영감) |
| **TiDB** | 39.9k | MySQL 호환 분산 SQL (PingCAP). HTAP(Hybrid Transactional/Analytical Processing) |
| **YugabyteDB** | 10.1k | PostgreSQL 호환 분산 SQL |
| **OceanBase** | 10k | 분산 SQL (Ant Group). 트랜잭션 + 분석 통합 |

### NoSQL

| 도구 | ★ | 설명 |
|---|---|---|
| **Redis** | 73.2k | 인메모리 캐시/데이터 스토어. 가장 인기 있는 KV 스토어 |
| **Valkey** | 24.9k | Redis 포크 (BSD 라이선스). Redis 라이선스 변경 후 Linux Foundation에서 포크 |
| **MongoDB** | 28.2k | 도큐먼트 DB. JSON 형태 데이터 저장/쿼리 |
| **Cassandra** | 9.6k | 대규모 분산 와이드 컬럼 스토어 (Facebook에서 개발) |
| **DragonflyDB** | 30.1k | Redis/Memcached 대체 (멀티스레드, 25배 처리량) |
| **Scylla** | 15.4k | C++로 재작성한 Cassandra 호환 DB (10배 성능) |

### 그래프 데이터베이스

| 도구 | ★ | 설명 |
|---|---|---|
| **Neo4j** | 16k | 가장 인기 있는 그래프 DB (Cypher 쿼리 언어) |
| **Dgraph** | 21.6k | 고성능 그래프 DB (GraphQL 네이티브) |
| **NebulaGraph** | 12k | 대규모 분산 그래프 DB |

### 분석용 데이터베이스 (OLAP)

| 도구 | ★ | 설명 |
|---|---|---|
| **ClickHouse** | 46.1k | 실시간 분석용 컬럼형 DB. 초고속 집계 쿼리 |
| **Presto** | 16.7k | 분산 SQL 쿼리 엔진. 빅데이터 소스를 SQL로 조회 (Facebook) |
| **Doris** | 15k | 실시간 분석 DB (Apache). MPP(Massively Parallel Processing) 아키텍처 |
| **Druid** | 13.9k | 실시간 OLAP DB. 이벤트 데이터 분석 특화 |
| **StarRocks** | 11.4k | 서브초 MPP 분석 DB. Doris 포크 |
| **Databend** | 9.2k | 클라우드 데이터 웨어하우스 (Rust, S3 네이티브) |

### 시계열 데이터베이스 (Time Series)

| 도구 | ★ | 설명 |
|---|---|---|
| **TDengine** | 24.7k | IoT/IIoT용 고성능 시계열 DB |
| **Timescale** | 22k | PostgreSQL 기반 시계열 DB. PostgreSQL 확장으로 설치 |

### 벡터 데이터베이스 (AI/LLM 시대 핵심)

| 도구 | ★ | 설명 |
|---|---|---|
| **Qdrant** | 29.2k | Rust 기반 고성능 벡터 DB. 필터링 + 벡터 검색 |
| **Weaviate** | 15.7k | 벡터 + 구조화 검색 통합 DB. GraphQL API |

> 벡터 DB는 텍스트/이미지를 고차원 벡터로 변환하여 저장하고,
> "의미적으로 유사한" 데이터를 검색하는 DB. LLM의 RAG(Retrieval-Augmented Generation)에서 핵심.

### K8s 운영 도구

| 도구 | ★ | 설명 |
|---|---|---|
| **Crunchy Postgres Operator** | 4.4k | PostgreSQL K8s Operator (HA, 백업, 복제) |
| **KubeBlocks** | 3k | K8s에서 다양한 DB 운영 Operator (관계형, NoSQL, 벡터, 스트리밍) |
| **Stolon** | 4.8k | 클라우드 네이티브 PostgreSQL HA(High Availability) |
