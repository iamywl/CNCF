package main

import (
	"fmt"
	"strings"
	"sort"
)

// =============================================================================
// gRPC Reflection 서비스 시뮬레이션
// =============================================================================
//
// gRPC Reflection은 서버가 자신의 서비스/메서드 정보를 동적으로 노출하는 기능이다.
// grpcurl, grpc_cli 등 도구가 이를 사용하여 서비스를 탐색한다.
//
// 핵심 개념:
//   - ServiceInfo: 등록된 서비스와 메서드 정보
//   - FileDescriptor: protobuf 파일 디스크립터 (스키마 정보)
//   - Symbol Resolution: 서비스/메서드/메시지 타입 이름으로 정보 조회
//
// 실제 코드 참조:
//   - reflection/serverreflection.go: 리플렉션 서비스 구현
//   - reflection/grpc_reflection_v1/reflection.proto: API 정의
// =============================================================================

// --- Protobuf 타입 시스템 시뮬레이션 ---

type FieldType int

const (
	TypeString  FieldType = iota
	TypeInt32
	TypeInt64
	TypeBool
	TypeDouble
	TypeBytes
	TypeMessage
	TypeEnum
	TypeRepeated
	TypeMap
)

func (t FieldType) String() string {
	names := []string{"string", "int32", "int64", "bool", "double", "bytes", "message", "enum", "repeated", "map"}
	if int(t) < len(names) {
		return names[t]
	}
	return "unknown"
}

type FieldDescriptor struct {
	Name       string
	Number     int
	Type       FieldType
	TypeName   string // message/enum 타입 이름
	IsRepeated bool
}

type MessageDescriptor struct {
	FullName string
	Fields   []FieldDescriptor
}

type EnumDescriptor struct {
	FullName string
	Values   map[string]int
}

type MethodDescriptor struct {
	Name           string
	InputType      string
	OutputType     string
	ClientStreaming bool
	ServerStreaming bool
}

type ServiceDescriptor struct {
	FullName string
	Methods  []MethodDescriptor
}

type FileDescriptor struct {
	Name       string
	Package    string
	Services   []ServiceDescriptor
	Messages   []MessageDescriptor
	Enums      []EnumDescriptor
	Dependency []string
}

// --- Reflection Server ---

type ReflectionServer struct {
	files    map[string]*FileDescriptor
	services map[string]*ServiceDescriptor
	messages map[string]*MessageDescriptor
	enums    map[string]*EnumDescriptor
}

func NewReflectionServer() *ReflectionServer {
	return &ReflectionServer{
		files:    make(map[string]*FileDescriptor),
		services: make(map[string]*ServiceDescriptor),
		messages: make(map[string]*MessageDescriptor),
		enums:    make(map[string]*EnumDescriptor),
	}
}

func (rs *ReflectionServer) RegisterFile(fd *FileDescriptor) {
	rs.files[fd.Name] = fd
	for i := range fd.Services {
		rs.services[fd.Services[i].FullName] = &fd.Services[i]
	}
	for i := range fd.Messages {
		rs.messages[fd.Messages[i].FullName] = &fd.Messages[i]
	}
	for i := range fd.Enums {
		rs.enums[fd.Enums[i].FullName] = &fd.Enums[i]
	}
}

// ListServices는 등록된 모든 서비스를 반환한다.
func (rs *ReflectionServer) ListServices() []string {
	var result []string
	for name := range rs.services {
		result = append(result, name)
	}
	sort.Strings(result)
	// 리플렉션 서비스 자체도 포함
	result = append(result, "grpc.reflection.v1.ServerReflection")
	return result
}

// FileContainingSymbol은 심볼이 정의된 파일을 반환한다.
func (rs *ReflectionServer) FileContainingSymbol(symbol string) (*FileDescriptor, error) {
	for _, fd := range rs.files {
		for _, svc := range fd.Services {
			if svc.FullName == symbol {
				return fd, nil
			}
			for _, m := range svc.Methods {
				fullMethod := svc.FullName + "." + m.Name
				if fullMethod == symbol {
					return fd, nil
				}
			}
		}
		for _, msg := range fd.Messages {
			if msg.FullName == symbol {
				return fd, nil
			}
		}
	}
	return nil, fmt.Errorf("symbol not found: %s", symbol)
}

// DescribeService는 서비스의 상세 정보를 출력한다.
func (rs *ReflectionServer) DescribeService(name string) {
	svc, ok := rs.services[name]
	if !ok {
		fmt.Printf("  Service not found: %s\n", name)
		return
	}
	fmt.Printf("  service %s {\n", svc.FullName)
	for _, m := range svc.Methods {
		streamPrefix := ""
		streamSuffix := ""
		if m.ClientStreaming {
			streamPrefix = "stream "
		}
		if m.ServerStreaming {
			streamSuffix = "stream "
		}
		fmt.Printf("    rpc %s (%s%s) returns (%s%s);\n",
			m.Name, streamPrefix, m.InputType, streamSuffix, m.OutputType)
	}
	fmt.Println("  }")
}

// DescribeMessage는 메시지 타입의 상세 정보를 출력한다.
func (rs *ReflectionServer) DescribeMessage(name string) {
	msg, ok := rs.messages[name]
	if !ok {
		fmt.Printf("  Message not found: %s\n", name)
		return
	}
	fmt.Printf("  message %s {\n", msg.FullName)
	for _, f := range msg.Fields {
		typeName := f.Type.String()
		if f.Type == TypeMessage || f.Type == TypeEnum {
			typeName = f.TypeName
		}
		repeated := ""
		if f.IsRepeated {
			repeated = "repeated "
		}
		fmt.Printf("    %s%s %s = %d;\n", repeated, typeName, f.Name, f.Number)
	}
	fmt.Println("  }")
}

// DescribeEnum은 enum 타입의 상세 정보를 출력한다.
func (rs *ReflectionServer) DescribeEnum(name string) {
	e, ok := rs.enums[name]
	if !ok {
		fmt.Printf("  Enum not found: %s\n", name)
		return
	}
	fmt.Printf("  enum %s {\n", e.FullName)
	for name, val := range e.Values {
		fmt.Printf("    %s = %d;\n", name, val)
	}
	fmt.Println("  }")
}

// --- 테스트 데이터 생성 ---

func buildTestSchema() *ReflectionServer {
	rs := NewReflectionServer()

	// helloworld.proto
	rs.RegisterFile(&FileDescriptor{
		Name:    "helloworld.proto",
		Package: "helloworld",
		Services: []ServiceDescriptor{
			{
				FullName: "helloworld.Greeter",
				Methods: []MethodDescriptor{
					{"SayHello", "helloworld.HelloRequest", "helloworld.HelloReply", false, false},
					{"SayHelloStream", "helloworld.HelloRequest", "helloworld.HelloReply", false, true},
				},
			},
		},
		Messages: []MessageDescriptor{
			{
				FullName: "helloworld.HelloRequest",
				Fields: []FieldDescriptor{
					{"name", 1, TypeString, "", false},
					{"greeting_count", 2, TypeInt32, "", false},
				},
			},
			{
				FullName: "helloworld.HelloReply",
				Fields: []FieldDescriptor{
					{"message", 1, TypeString, "", false},
					{"timestamp", 2, TypeInt64, "", false},
				},
			},
		},
	})

	// route_guide.proto
	rs.RegisterFile(&FileDescriptor{
		Name:    "route_guide.proto",
		Package: "routeguide",
		Services: []ServiceDescriptor{
			{
				FullName: "routeguide.RouteGuide",
				Methods: []MethodDescriptor{
					{"GetFeature", "routeguide.Point", "routeguide.Feature", false, false},
					{"ListFeatures", "routeguide.Rectangle", "routeguide.Feature", false, true},
					{"RecordRoute", "routeguide.Point", "routeguide.RouteSummary", true, false},
					{"RouteChat", "routeguide.RouteNote", "routeguide.RouteNote", true, true},
				},
			},
		},
		Messages: []MessageDescriptor{
			{
				FullName: "routeguide.Point",
				Fields: []FieldDescriptor{
					{"latitude", 1, TypeInt32, "", false},
					{"longitude", 2, TypeInt32, "", false},
				},
			},
			{
				FullName: "routeguide.Feature",
				Fields: []FieldDescriptor{
					{"name", 1, TypeString, "", false},
					{"location", 2, TypeMessage, "routeguide.Point", false},
				},
			},
			{
				FullName: "routeguide.Rectangle",
				Fields: []FieldDescriptor{
					{"lo", 1, TypeMessage, "routeguide.Point", false},
					{"hi", 2, TypeMessage, "routeguide.Point", false},
				},
			},
			{
				FullName: "routeguide.RouteSummary",
				Fields: []FieldDescriptor{
					{"point_count", 1, TypeInt32, "", false},
					{"feature_count", 2, TypeInt32, "", false},
					{"distance", 3, TypeInt32, "", false},
					{"elapsed_time", 4, TypeInt32, "", false},
				},
			},
			{
				FullName: "routeguide.RouteNote",
				Fields: []FieldDescriptor{
					{"location", 1, TypeMessage, "routeguide.Point", false},
					{"message", 2, TypeString, "", false},
				},
			},
		},
		Enums: []EnumDescriptor{
			{
				FullName: "routeguide.FeatureType",
				Values:   map[string]int{"UNKNOWN": 0, "RESTAURANT": 1, "PARK": 2, "MUSEUM": 3},
			},
		},
	})

	// health.proto
	rs.RegisterFile(&FileDescriptor{
		Name:    "grpc/health/v1/health.proto",
		Package: "grpc.health.v1",
		Services: []ServiceDescriptor{
			{
				FullName: "grpc.health.v1.Health",
				Methods: []MethodDescriptor{
					{"Check", "grpc.health.v1.HealthCheckRequest", "grpc.health.v1.HealthCheckResponse", false, false},
					{"Watch", "grpc.health.v1.HealthCheckRequest", "grpc.health.v1.HealthCheckResponse", false, true},
				},
			},
		},
		Messages: []MessageDescriptor{
			{
				FullName: "grpc.health.v1.HealthCheckRequest",
				Fields:   []FieldDescriptor{{"service", 1, TypeString, "", false}},
			},
			{
				FullName: "grpc.health.v1.HealthCheckResponse",
				Fields:   []FieldDescriptor{{"status", 1, TypeEnum, "grpc.health.v1.HealthCheckResponse.ServingStatus", false}},
			},
		},
		Enums: []EnumDescriptor{
			{
				FullName: "grpc.health.v1.HealthCheckResponse.ServingStatus",
				Values:   map[string]int{"UNKNOWN": 0, "SERVING": 1, "NOT_SERVING": 2, "SERVICE_UNKNOWN": 3},
			},
		},
	})

	return rs
}

func main() {
	fmt.Println("=== gRPC Reflection 서비스 시뮬레이션 ===")
	fmt.Println()

	rs := buildTestSchema()

	// --- 서비스 목록 ---
	fmt.Println("[1] ListServices (grpcurl -plaintext localhost:50051 list)")
	fmt.Println(strings.Repeat("-", 60))
	services := rs.ListServices()
	for _, svc := range services {
		fmt.Printf("  %s\n", svc)
	}
	fmt.Println()

	// --- 서비스 설명 ---
	fmt.Println("[2] DescribeService (grpcurl describe <service>)")
	fmt.Println(strings.Repeat("-", 60))
	for _, name := range []string{"helloworld.Greeter", "routeguide.RouteGuide", "grpc.health.v1.Health"} {
		fmt.Printf("\n")
		rs.DescribeService(name)
	}
	fmt.Println()

	// --- 메시지 타입 설명 ---
	fmt.Println("[3] DescribeMessage (메시지 타입 스키마)")
	fmt.Println(strings.Repeat("-", 60))
	for _, name := range []string{
		"helloworld.HelloRequest", "helloworld.HelloReply",
		"routeguide.Point", "routeguide.Feature", "routeguide.RouteSummary",
	} {
		fmt.Printf("\n")
		rs.DescribeMessage(name)
	}
	fmt.Println()

	// --- Enum 설명 ---
	fmt.Println("[4] DescribeEnum")
	fmt.Println(strings.Repeat("-", 60))
	rs.DescribeEnum("routeguide.FeatureType")
	rs.DescribeEnum("grpc.health.v1.HealthCheckResponse.ServingStatus")
	fmt.Println()

	// --- 심볼로 파일 찾기 ---
	fmt.Println("[5] FileContainingSymbol")
	fmt.Println(strings.Repeat("-", 60))
	symbols := []string{
		"helloworld.Greeter",
		"routeguide.RouteGuide",
		"routeguide.Point",
		"grpc.health.v1.Health",
		"nonexistent.Service",
	}
	for _, sym := range symbols {
		fd, err := rs.FileContainingSymbol(sym)
		if err != nil {
			fmt.Printf("  %s -> ERROR: %v\n", sym, err)
		} else {
			fmt.Printf("  %s -> %s (package: %s)\n", sym, fd.Name, fd.Package)
		}
	}
	fmt.Println()

	// --- grpcurl 시뮬레이션 ---
	fmt.Println("[6] grpcurl 스타일 출력")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  $ grpcurl -plaintext localhost:50051 list")
	for _, svc := range rs.ListServices() {
		fmt.Printf("  %s\n", svc)
	}
	fmt.Println()

	fmt.Println("  $ grpcurl -plaintext localhost:50051 list routeguide.RouteGuide")
	if svc, ok := rs.services["routeguide.RouteGuide"]; ok {
		for _, m := range svc.Methods {
			fmt.Printf("  %s.%s\n", svc.FullName, m.Name)
		}
	}
	fmt.Println()

	fmt.Println("  $ grpcurl -plaintext localhost:50051 describe routeguide.Feature")
	rs.DescribeMessage("routeguide.Feature")
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
