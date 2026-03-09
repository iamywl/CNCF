// poc-28-live-pipeline: Grafana 라이브 파이프라인 시뮬레이션
//
// 핵심 개념:
//   - 채널 규칙 매칭 (패턴 기반)
//   - 데이터 파이프라인: DataOutput → Convert → Process → Output
//   - 채널 라우팅 (출력에서 다른 채널로 전달)
//   - 재귀 방지 (visitedChannels)
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// --- 파이프라인 인터페이스 ---

type Vars struct {
	Channel string
	Scope   string
	Path    string
}

type ChannelFrame struct {
	Channel string
	Data    map[string]interface{}
}

type Converter interface {
	Type() string
	Convert(vars Vars, body []byte) ([]*ChannelFrame, error)
}

type FrameProcessor interface {
	Type() string
	Process(vars Vars, data map[string]interface{}) (map[string]interface{}, error)
}

type FrameOutputter interface {
	Type() string
	Output(vars Vars, data map[string]interface{}) ([]*ChannelFrame, error)
}

// --- 규칙 ---

type LiveChannelRule struct {
	Pattern         string
	Converter       Converter
	FrameProcessors []FrameProcessor
	FrameOutputters []FrameOutputter
}

// --- 구현체: JSON 변환기 ---

type JSONConverter struct{}

func (c *JSONConverter) Type() string { return "json_auto" }
func (c *JSONConverter) Convert(vars Vars, body []byte) ([]*ChannelFrame, error) {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return []*ChannelFrame{{Data: data}}, nil
}

// --- 구현체: 필드 필터 프로세서 ---

type KeepFieldProcessor struct {
	Fields []string
}

func (p *KeepFieldProcessor) Type() string { return "keep_fields" }
func (p *KeepFieldProcessor) Process(vars Vars, data map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	fieldSet := make(map[string]bool)
	for _, f := range p.Fields {
		fieldSet[f] = true
	}
	for k, v := range data {
		if fieldSet[k] {
			result[k] = v
		}
	}
	return result, nil
}

// --- 구현체: 임계값 출력기 ---

type ThresholdOutputter struct {
	Field     string
	Threshold float64
	AlertCh   string // 임계값 초과 시 라우팅할 채널
}

func (o *ThresholdOutputter) Type() string { return "threshold" }
func (o *ThresholdOutputter) Output(vars Vars, data map[string]interface{}) ([]*ChannelFrame, error) {
	val, ok := data[o.Field]
	if !ok {
		return nil, nil
	}
	fval, ok := val.(float64)
	if !ok {
		return nil, nil
	}
	if fval > o.Threshold {
		fmt.Printf("    [임계값 초과] %s=%.1f > %.1f → 채널 %s로 라우팅\n",
			o.Field, fval, o.Threshold, o.AlertCh)
		return []*ChannelFrame{{Channel: o.AlertCh, Data: data}}, nil
	}
	return nil, nil
}

// --- 구현체: 로그 출력기 ---

type LogOutputter struct{}

func (o *LogOutputter) Type() string { return "log" }
func (o *LogOutputter) Output(vars Vars, data map[string]interface{}) ([]*ChannelFrame, error) {
	dataJSON, _ := json.Marshal(data)
	fmt.Printf("    [출력] 채널=%s, 데이터=%s\n", vars.Channel, string(dataJSON))
	return nil, nil
}

// --- 규칙 캐시 (패턴 매칭) ---

type RuleCache struct {
	rules []LiveChannelRule
}

func NewRuleCache() *RuleCache {
	return &RuleCache{}
}

func (c *RuleCache) AddRule(rule LiveChannelRule) {
	c.rules = append(c.rules, rule)
}

func (c *RuleCache) Get(channel string) (*LiveChannelRule, bool) {
	for _, rule := range c.rules {
		if matchPattern(rule.Pattern, channel) {
			return &rule, true
		}
	}
	return nil, false
}

func matchPattern(pattern, channel string) bool {
	// 간단한 와일드카드 매칭 (실제는 httprouter 스타일)
	if pattern == channel {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := pattern[:len(pattern)-2]
		return strings.HasPrefix(channel, prefix)
	}
	return false
}

// --- 파이프라인 ---

type Pipeline struct {
	ruleCache *RuleCache
}

func NewPipeline(cache *RuleCache) *Pipeline {
	return &Pipeline{ruleCache: cache}
}

func (p *Pipeline) ProcessInput(channel string, body []byte) error {
	visited := make(map[string]bool)
	return p.processInput(channel, body, visited)
}

func (p *Pipeline) processInput(channel string, body []byte, visited map[string]bool) error {
	// 재귀 방지
	if visited[channel] {
		return fmt.Errorf("채널 재귀 감지: %s", channel)
	}
	visited[channel] = true

	rule, ok := p.ruleCache.Get(channel)
	if !ok {
		return fmt.Errorf("규칙 없음: %s", channel)
	}

	fmt.Printf("  [파이프라인] 채널=%s\n", channel)

	// 1. 변환
	if rule.Converter == nil {
		return nil
	}
	vars := Vars{Channel: channel}
	frames, err := rule.Converter.Convert(vars, body)
	if err != nil {
		return err
	}

	// 2. 각 프레임 처리
	for _, frame := range frames {
		data := frame.Data

		// 프로세서 적용
		for _, proc := range rule.FrameProcessors {
			data, err = proc.Process(vars, data)
			if err != nil {
				return err
			}
			if data == nil {
				break
			}
		}
		if data == nil {
			continue
		}

		// 출력
		for _, out := range rule.FrameOutputters {
			resultFrames, err := out.Output(vars, data)
			if err != nil {
				return err
			}
			// 라우팅된 채널 처리
			for _, rf := range resultFrames {
				if rf.Channel != "" {
					rfBody, _ := json.Marshal(rf.Data)
					if err := p.processInput(rf.Channel, rfBody, visited); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// --- 메인 ---

func main() {
	fmt.Println("=== Grafana 라이브 파이프라인 시뮬레이션 ===")
	fmt.Println()

	cache := NewRuleCache()

	// 규칙 1: 센서 데이터 수집
	cache.AddRule(LiveChannelRule{
		Pattern:   "stream/sensor/*",
		Converter: &JSONConverter{},
		FrameProcessors: []FrameProcessor{
			&KeepFieldProcessor{Fields: []string{"temperature", "humidity", "host"}},
		},
		FrameOutputters: []FrameOutputter{
			&LogOutputter{},
			&ThresholdOutputter{Field: "temperature", Threshold: 80.0, AlertCh: "stream/alerts/temperature"},
		},
	})

	// 규칙 2: 알림 채널
	cache.AddRule(LiveChannelRule{
		Pattern:   "stream/alerts/*",
		Converter: &JSONConverter{},
		FrameOutputters: []FrameOutputter{
			&LogOutputter{},
		},
	})

	pipeline := NewPipeline(cache)

	// 1. 정상 데이터
	fmt.Println("--- 1. 정상 센서 데이터 ---")
	data1, _ := json.Marshal(map[string]interface{}{
		"temperature": 65.5,
		"humidity":    45.0,
		"host":        "server-01",
		"timestamp":   time.Now().Unix(),
	})
	pipeline.ProcessInput("stream/sensor/room1", data1)

	// 2. 임계값 초과 데이터
	fmt.Println()
	fmt.Println("--- 2. 임계값 초과 (temperature > 80) ---")
	data2, _ := json.Marshal(map[string]interface{}{
		"temperature": 92.3,
		"humidity":    30.0,
		"host":        "server-02",
		"extra_field": "필터링됨",
	})
	pipeline.ProcessInput("stream/sensor/room2", data2)

	// 3. 규칙 없는 채널
	fmt.Println()
	fmt.Println("--- 3. 규칙 없는 채널 ---")
	err := pipeline.ProcessInput("stream/unknown/test", []byte("{}"))
	fmt.Printf("  결과: %v\n", err)

	// 4. 재귀 방지 테스트
	fmt.Println()
	fmt.Println("--- 4. 재귀 방지 테스트 ---")
	cache.AddRule(LiveChannelRule{
		Pattern:   "stream/loop",
		Converter: &JSONConverter{},
		FrameOutputters: []FrameOutputter{
			&ThresholdOutputter{Field: "val", Threshold: 0, AlertCh: "stream/loop"},
		},
	})
	data3, _ := json.Marshal(map[string]interface{}{"val": 1.0})
	err = pipeline.ProcessInput("stream/loop", data3)
	fmt.Printf("  결과: %v\n", err)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
