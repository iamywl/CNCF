package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes 워크로드 컨트롤러 시뮬레이션
// StatefulSet, DaemonSet, Job, CronJob의 핵심 알고리즘 구현
// 참조:
//   - StatefulSet: pkg/controller/statefulset/stateful_set_control.go
//   - DaemonSet: pkg/controller/daemon/daemon_controller.go
//   - Job: pkg/controller/job/job_controller.go
//   - CronJob: pkg/controller/cronjob/cronjob_controllerv2.go
// =============================================================================

// --- StatefulSet 시뮬레이션 ---

type StatefulSetPod struct {
	Name     string
	Ordinal  int
	Ready    bool
	Revision string
	PVCName  string
}

type StatefulSet struct {
	Name            string
	Replicas        int
	CurrentRevision string
	UpdateRevision  string
	UpdateStrategy  string // "RollingUpdate" or "OnDelete"
	Pods            []*StatefulSetPod
}

func (ss *StatefulSet) Reconcile() {
	fmt.Printf("  [StatefulSet] '%s' reconcile (replicas=%d, strategy=%s)\n",
		ss.Name, ss.Replicas, ss.UpdateStrategy)

	// 1. ordinal 순서로 정렬
	sort.Slice(ss.Pods, func(i, j int) bool {
		return ss.Pods[i].Ordinal < ss.Pods[j].Ordinal
	})

	// 2. Scale up: 부족한 Pod 생성 (순서대로)
	for i := len(ss.Pods); i < ss.Replicas; i++ {
		pod := &StatefulSetPod{
			Name:     fmt.Sprintf("%s-%d", ss.Name, i),
			Ordinal:  i,
			Ready:    false,
			Revision: ss.UpdateRevision,
			PVCName:  fmt.Sprintf("data-%s-%d", ss.Name, i),
		}
		ss.Pods = append(ss.Pods, pod)
		fmt.Printf("    생성: %s (ordinal=%d, PVC=%s)\n", pod.Name, pod.Ordinal, pod.PVCName)

		// Monotonic: 이전 Pod이 Ready여야 다음 생성
		if i > 0 && !ss.Pods[i-1].Ready {
			fmt.Printf("    ⏸ ordinal %d가 Ready가 아님 → 대기\n", i-1)
			break
		}
		pod.Ready = true // 시뮬레이션: 즉시 Ready
	}

	// 3. Scale down: 역순으로 삭제
	for len(ss.Pods) > ss.Replicas {
		last := ss.Pods[len(ss.Pods)-1]
		fmt.Printf("    삭제: %s (ordinal=%d, PVC 유지)\n", last.Name, last.Ordinal)
		ss.Pods = ss.Pods[:len(ss.Pods)-1]
	}

	// 4. Rolling Update (역순)
	if ss.UpdateStrategy == "RollingUpdate" {
		for i := len(ss.Pods) - 1; i >= 0; i-- {
			pod := ss.Pods[i]
			if pod.Revision != ss.UpdateRevision {
				fmt.Printf("    업데이트: %s (%s → %s)\n", pod.Name, pod.Revision, ss.UpdateRevision)
				pod.Revision = ss.UpdateRevision
				pod.Ready = true
			}
		}
	}
}

// --- DaemonSet 시뮬레이션 ---

type DaemonNode struct {
	Name   string
	Taints []string
	Pod    *string // nil = Pod 없음
}

type DaemonSet struct {
	Name        string
	Tolerations []string
	Nodes       []*DaemonNode
}

func (ds *DaemonSet) Manage() {
	fmt.Printf("  [DaemonSet] '%s' manage (%d nodes)\n", ds.Name, len(ds.Nodes))

	var needPod, deletePod []string

	for _, node := range ds.Nodes {
		shouldRun := ds.shouldRunOnNode(node)
		hasPod := node.Pod != nil

		switch {
		case shouldRun && !hasPod:
			needPod = append(needPod, node.Name)
		case !shouldRun && hasPod:
			deletePod = append(deletePod, node.Name)
		}
	}

	// Slow Start Batch로 생성
	ds.slowStartBatch(needPod)

	// 삭제
	for _, nodeName := range deletePod {
		for _, node := range ds.Nodes {
			if node.Name == nodeName {
				fmt.Printf("    삭제: node=%s (toleration 불일치)\n", nodeName)
				node.Pod = nil
			}
		}
	}
}

func (ds *DaemonSet) shouldRunOnNode(node *DaemonNode) bool {
	for _, taint := range node.Taints {
		tolerated := false
		for _, t := range ds.Tolerations {
			if t == taint {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// Slow Start: 초기 1개 → 성공 시 2배씩 증가
func (ds *DaemonSet) slowStartBatch(nodes []string) {
	batchSize := 1
	for pos := 0; pos < len(nodes); {
		end := pos + batchSize
		if end > len(nodes) {
			end = len(nodes)
		}
		batch := nodes[pos:end]
		fmt.Printf("    Slow Start batch (size=%d): ", len(batch))
		for _, nodeName := range batch {
			for _, node := range ds.Nodes {
				if node.Name == nodeName {
					podName := fmt.Sprintf("%s-%5s", ds.Name, nodeName[len(nodeName)-5:])
					node.Pod = &podName
				}
			}
		}
		fmt.Printf("%v\n", batch)
		pos = end
		batchSize *= 2 // 성공 시 2배
	}
}

// --- Job 시뮬레이션 ---

type Job struct {
	Name         string
	Completions  int
	Parallelism  int
	BackoffLimit int
	Succeeded    int
	Failed       int
	Active       int
}

func (j *Job) SyncJob() {
	fmt.Printf("  [Job] '%s' sync (completions=%d, parallelism=%d, backoffLimit=%d)\n",
		j.Name, j.Completions, j.Parallelism, j.BackoffLimit)

	for j.Succeeded < j.Completions && j.Failed <= j.BackoffLimit {
		// 활성 Pod 수 결정
		needed := j.Completions - j.Succeeded - j.Active
		toCreate := j.Parallelism - j.Active
		if toCreate > needed {
			toCreate = needed
		}
		if toCreate < 0 {
			toCreate = 0
		}

		j.Active += toCreate
		if toCreate > 0 {
			fmt.Printf("    Pod %d개 생성 (active=%d)\n", toCreate, j.Active)
		}

		// 시뮬레이션: 일부 성공, 일부 실패
		for i := 0; i < j.Active; i++ {
			if rand.Float64() > 0.2 { // 80% 성공률
				j.Succeeded++
				j.Active--
				fmt.Printf("    Pod 성공 (succeeded=%d/%d)\n", j.Succeeded, j.Completions)
			} else {
				j.Failed++
				j.Active--
				fmt.Printf("    Pod 실패 (failed=%d, backoffLimit=%d)\n", j.Failed, j.BackoffLimit)
			}

			if j.Succeeded >= j.Completions {
				break
			}
			if j.Failed > j.BackoffLimit {
				break
			}
		}
	}

	if j.Succeeded >= j.Completions {
		fmt.Printf("    ✓ Job 완료 (succeeded=%d)\n", j.Succeeded)
	} else {
		fmt.Printf("    ✗ Job 실패 (backoffLimit 초과: failed=%d)\n", j.Failed)
	}
}

// --- CronJob 시뮬레이션 ---

type ConcurrencyPolicy string

const (
	AllowConcurrent   ConcurrencyPolicy = "Allow"
	ForbidConcurrent  ConcurrencyPolicy = "Forbid"
	ReplaceConcurrent ConcurrencyPolicy = "Replace"
)

type CronJob struct {
	mu                sync.Mutex
	Name              string
	Schedule          string
	ConcurrencyPolicy ConcurrencyPolicy
	Suspend           bool
	HistoryLimit      int
	ActiveJobs        []string
	CompletedJobs     []string
}

func (cj *CronJob) Sync(now time.Time) {
	cj.mu.Lock()
	defer cj.mu.Unlock()

	fmt.Printf("  [CronJob] '%s' sync (schedule=%s, policy=%s, suspend=%v)\n",
		cj.Name, cj.Schedule, cj.ConcurrencyPolicy, cj.Suspend)

	if cj.Suspend {
		fmt.Printf("    Suspended → 스킵\n")
		return
	}

	// 동시 정책 확인
	switch cj.ConcurrencyPolicy {
	case ForbidConcurrent:
		if len(cj.ActiveJobs) > 0 {
			fmt.Printf("    Forbid: 활성 Job %v 존재 → 스킵\n", cj.ActiveJobs)
			return
		}
	case ReplaceConcurrent:
		if len(cj.ActiveJobs) > 0 {
			fmt.Printf("    Replace: 기존 Job %v 삭제\n", cj.ActiveJobs)
			cj.ActiveJobs = nil
		}
	}

	// 새 Job 생성
	jobName := fmt.Sprintf("%s-%d", cj.Name, now.Unix())
	cj.ActiveJobs = append(cj.ActiveJobs, jobName)
	fmt.Printf("    새 Job 생성: %s\n", jobName)

	// 시뮬레이션: Job 즉시 완료
	cj.CompletedJobs = append(cj.CompletedJobs, jobName)
	cj.ActiveJobs = removeString(cj.ActiveJobs, jobName)

	// 히스토리 정리
	cj.cleanupHistory()
}

func (cj *CronJob) cleanupHistory() {
	if len(cj.CompletedJobs) > cj.HistoryLimit {
		removed := len(cj.CompletedJobs) - cj.HistoryLimit
		cj.CompletedJobs = cj.CompletedJobs[removed:]
		fmt.Printf("    히스토리 정리: %d개 삭제 (limit=%d)\n", removed, cj.HistoryLimit)
	}
}

func removeString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	rand.New(rand.NewSource(42))
	fmt.Println("=== Kubernetes 워크로드 컨트롤러 시뮬레이션 ===")
	fmt.Println()

	// 1. StatefulSet
	demo1_StatefulSet()

	// 2. DaemonSet
	demo2_DaemonSet()

	// 3. Job
	demo3_Job()

	// 4. CronJob
	demo4_CronJob()

	// 비교 테이블
	printComparison()
}

func demo1_StatefulSet() {
	fmt.Println("--- 1. StatefulSet (순서 보장 + PVC) ---")

	ss := &StatefulSet{
		Name:            "mysql",
		Replicas:        3,
		CurrentRevision: "rev-1",
		UpdateRevision:  "rev-2",
		UpdateStrategy:  "RollingUpdate",
		Pods: []*StatefulSetPod{
			{Name: "mysql-0", Ordinal: 0, Ready: true, Revision: "rev-1", PVCName: "data-mysql-0"},
			{Name: "mysql-1", Ordinal: 1, Ready: true, Revision: "rev-1", PVCName: "data-mysql-1"},
		},
	}
	ss.Reconcile()
	fmt.Println()
}

func demo2_DaemonSet() {
	fmt.Println("--- 2. DaemonSet (노드당 하나 + Toleration) ---")

	ds := &DaemonSet{
		Name:        "fluentd",
		Tolerations: []string{"node-role.kubernetes.io/control-plane"},
		Nodes: []*DaemonNode{
			{Name: "master-1", Taints: []string{"node-role.kubernetes.io/control-plane"}},
			{Name: "worker-1", Taints: nil},
			{Name: "worker-2", Taints: nil},
			{Name: "worker-3", Taints: []string{"gpu-only"}}, // toleration 없음
		},
	}
	ds.Manage()
	fmt.Println()
}

func demo3_Job() {
	fmt.Println("--- 3. Job (완료 횟수 + 병렬 + 백오프) ---")

	job := &Job{
		Name:         "batch-process",
		Completions:  5,
		Parallelism:  2,
		BackoffLimit: 3,
	}
	job.SyncJob()
	fmt.Println()
}

func demo4_CronJob() {
	fmt.Println("--- 4. CronJob (스케줄 + 동시 정책) ---")

	now := time.Now()

	// Allow 정책
	cj1 := &CronJob{
		Name:              "report-allow",
		Schedule:          "*/5 * * * *",
		ConcurrencyPolicy: AllowConcurrent,
		HistoryLimit:      3,
	}
	cj1.Sync(now)

	// Forbid 정책 (활성 Job 존재)
	cj2 := &CronJob{
		Name:              "report-forbid",
		Schedule:          "*/5 * * * *",
		ConcurrencyPolicy: ForbidConcurrent,
		ActiveJobs:        []string{"report-forbid-prev"},
		HistoryLimit:      3,
	}
	cj2.Sync(now)

	// Replace 정책
	cj3 := &CronJob{
		Name:              "report-replace",
		Schedule:          "*/5 * * * *",
		ConcurrencyPolicy: ReplaceConcurrent,
		ActiveJobs:        []string{"report-replace-prev"},
		HistoryLimit:      3,
	}
	cj3.Sync(now)

	fmt.Println()
}

func printComparison() {
	fmt.Println("=== 워크로드 컨트롤러 비교 ===")
	fmt.Println()

	header := fmt.Sprintf("  %-15s %-15s %-18s %-15s", "특성", "StatefulSet", "DaemonSet", "Job")
	fmt.Println(header)
	fmt.Println("  " + strings.Repeat("-", 63))

	rows := [][]string{
		{"목표", "상태 보유/순서", "노드당 하나", "완료까지 실행"},
		{"Pod 순서", "보장 (ordinal)", "무관", "무관"},
		{"Pod 이름", "name-ordinal", "랜덤 해시", "랜덤 해시"},
		{"PVC", "VolumeClaimTpl", "없음", "없음"},
		{"Update", "Rolling/OnDelete", "Rolling/maxSurge", "삭제→재생성"},
	}

	for _, row := range rows {
		fmt.Printf("  %-15s %-15s %-18s %-15s\n", row[0], row[1], row[2], row[3])
	}

	fmt.Println()
	fmt.Println("소스코드 참조:")
	refs := []string{
		"  - StatefulSet: pkg/controller/statefulset/stateful_set_control.go",
		"  - DaemonSet:   pkg/controller/daemon/daemon_controller.go",
		"  - Job:         pkg/controller/job/job_controller.go",
		"  - CronJob:     pkg/controller/cronjob/cronjob_controllerv2.go",
	}
	for _, r := range refs {
		fmt.Println(r)
	}
}
