# PoC-12: Helm 리포지토리 인덱스

## 개요

Helm 차트 리포지토리의 핵심은 `index.yaml` 파일로, 리포지토리에서 사용 가능한 모든 차트와 버전 정보를 담고 있다. 이 PoC는 IndexFile 구조, 차트 검색, 인덱스 생성 및 병합을 시뮬레이션한다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/repo/v1/index.go` | IndexFile, ChartVersion, SortEntries, Get, Merge, IndexDirectory |
| `pkg/repo/v1/chartrepo.go` | ChartRepository, DownloadIndexFile |

## 핵심 개념

### 1. IndexFile 구조
- `apiVersion`: v1
- `entries`: chart name -> ChartVersions (SemVer 내림차순)
- `generated`: 생성 시각

### 2. ChartVersion 정렬
- `sort.Sort(sort.Reverse(versions))` - 최신 버전이 0번 인덱스
- prerelease 버전은 안정 버전보다 낮게 평가

### 3. Get 검색 로직
- 빈 버전: 안정 최신 (prerelease 제외)
- 정확한 버전: 문자열 매치

### 4. Merge 로직
- name+version 조합으로 중복 검사
- 새 엔트리만 추가, 기존 레코드 유지

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. index.yaml 구조 설명
2. 인덱스 생성 (차트 패키지 → IndexFile)
3. 차트 검색 (이름, 버전, 키워드)
4. 인덱스 병합 (helm repo index --merge)
5. JSON 직렬화 출력
6. 리포지토리 인덱스 아키텍처 다이어그램
