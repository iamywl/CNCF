# API Machinery 심화

## 1. 개요 - API Machinery가 해결하는 문제

Kubernetes는 수십 개의 API 리소스(Pod, Service, Deployment 등)를 관리하며, 각 리소스는 시간이 지남에 따라 v1alpha1 -> v1beta1 -> v1처럼 여러 버전을 거친다. 이 과정에서 다음과 같은 근본적인 문제가 발생한다:

1. **타입 안전성**: Go의 정적 타입 시스템에서 수백 가지 API 객체를 통일된 방식으로 다루려면?
2. **버전 호환성**: 클라이언트가 v1beta1로 보낸 요청을 v1 스토리지 형식으로 저장하려면?
3. **직렬화 통일**: JSON, YAML, Protobuf 등 다양한 형식을 일관되게 처리하려면?
4. **패치 전략**: 부분 업데이트 시 배열 필드를 교체할지 병합할지 어떻게 결정하려면?
5. **필드 소유권**: 여러 컨트롤러가 같은 객체의 서로 다른 필드를 수정할 때 충돌을 어떻게 방지하려면?
6. **확장성**: CRD나 API Aggregation으로 새 API를 추가할 때 기존 인프라를 재사용하려면?

**API Machinery**(`k8s.io/apimachinery`)는 이 모든 문제를 해결하는 기반 라이브러리다. Kubernetes API 서버뿐 아니라 client-go, kubectl, 커스텀 컨트롤러까지 모두 이 라이브러리에 의존한다.

```
+------------------------------------------------------------------+
|                     Kubernetes Ecosystem                         |
|                                                                  |
|  +-------------+  +-------------+  +-------------+  +----------+ |
|  | kube-apiserver| | client-go   | | kubectl     | | 커스텀    | |
|  |             | |             | |             | | 컨트롤러 | |
|  +------+------+  +------+------+  +------+------+  +----+-----+ |
|         |                |                |              |       |
|         +-------+--------+--------+-------+--------------+       |
|                 |                                                 |
|         +-------v-----------+                                    |
|         |  API Machinery    |                                    |
|         |                   |                                    |
|         | - runtime.Object  |                                    |
|         | - Scheme          |                                    |
|         | - Serialization   |                                    |
|         | - Conversion      |                                    |
|         | - Strategic Patch |                                    |
|         | - Field Manager   |                                    |
|         | - Discovery       |                                    |
|         +-------------------+                                    |
+------------------------------------------------------------------+
```

### 핵심 패키지 구조

| 패키지 | 위치 | 역할 |
|--------|------|------|
| `runtime` | `apimachinery/pkg/runtime/` | Object 인터페이스, Scheme, 직렬화 |
| `runtime/schema` | `apimachinery/pkg/runtime/schema/` | GVK/GVR 타입 정의 |
| `runtime/serializer` | `apimachinery/pkg/runtime/serializer/` | JSON/YAML/Protobuf 코덱 |
| `runtime/serializer/versioning` | `apimachinery/pkg/runtime/serializer/versioning/` | 버전 변환 코덱 |
| `strategicpatch` | `apimachinery/pkg/util/strategicpatch/` | Strategic Merge Patch |
| `managedfields` | `apimachinery/pkg/util/managedfields/` | Server-Side Apply |
| `apis/meta/v1` | `apimachinery/pkg/apis/meta/v1/` | TypeMeta, ObjectMeta, ListMeta |
| `discovery` | `client-go/discovery/` | API Discovery 클라이언트 |
| `openapi` | `client-go/openapi/` | OpenAPI 클라이언트 |
| `kube-aggregator` | `kube-aggregator/pkg/apiserver/` | API Aggregation Layer |

---

## 2. runtime.Object 인터페이스

### 2.1 왜 runtime.Object가 필요한가

Go에는 제네릭이 도입되기 전(Go 1.18 이전)부터 Kubernetes가 개발되었다. 수백 개의 API 타입(Pod, Service, Deployment, Node, ...)을 직렬화, 변환, 저장하는 코드에서 일일이 타입을 나열할 수 없다. 하나의 공통 인터페이스가 필요했고, 그것이 `runtime.Object`다.

### 2.2 인터페이스 정의

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/interfaces.go (줄 337-340)
type Object interface {
    GetObjectKind() schema.ObjectKind
    DeepCopyObject() Object
}
```

단 2개의 메서드만 요구한다:

| 메서드 | 역할 | 왜 필요한가 |
|--------|------|-------------|
| `GetObjectKind()` | 객체의 GVK(Group/Version/Kind)를 반환 | 직렬화할 때 `apiVersion`/`kind` 필드를 설정/조회하기 위해 |
| `DeepCopyObject()` | 객체의 깊은 복사본 반환 | 변환(conversion) 시 원본을 보존하기 위해 |

### 2.3 ObjectKind 인터페이스

`GetObjectKind()`이 반환하는 `schema.ObjectKind`는 다음과 같다:

```go
// schema.ObjectKind (interfaces.go에서 정의)
type ObjectKind interface {
    SetGroupVersionKind(kind GroupVersionKind)
    GroupVersionKind() GroupVersionKind
}
```

이 인터페이스를 통해 직렬화 프레임워크는 **어떤 구체 타입도 알 필요 없이** 객체의 종류를 설정하고 조회할 수 있다.

### 2.4 TypeMeta - 모든 API 객체의 공통 필드

모든 Kubernetes API 객체는 `TypeMeta`를 임베딩하여 `ObjectKind` 인터페이스를 자동으로 구현한다:

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/types.go (줄 38-43)
type TypeMeta struct {
    APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty" protobuf:"bytes,1,opt,name=apiVersion"`
    Kind       string `json:"kind,omitempty" yaml:"kind,omitempty" protobuf:"bytes,2,opt,name=kind"`
}
```

그리고 register.go에서 `ObjectKind` 인터페이스를 구현한다:

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/register.go (줄 21-31)
func (obj *TypeMeta) SetGroupVersionKind(gvk schema.GroupVersionKind) {
    obj.APIVersion, obj.Kind = gvk.ToAPIVersionAndKind()
}

func (obj *TypeMeta) GroupVersionKind() schema.GroupVersionKind {
    return schema.FromAPIVersionAndKind(obj.APIVersion, obj.Kind)
}

func (obj *TypeMeta) GetObjectKind() schema.ObjectKind { return obj }
```

**왜 TypeMeta가 JSON 태그를 가지는가?** Wire 포맷(JSON/YAML)에서 `apiVersion`과 `kind` 필드가 최상위에 나타나야 하기 때문이다. 이 두 필드를 통해 디시리얼라이저가 바이트 스트림을 보고 "이것은 apps/v1의 Deployment다"라고 판단할 수 있다.

### 2.5 meta/v1의 TypeMeta와 ListMeta

`k8s.io/apimachinery/pkg/apis/meta/v1` 패키지에도 `TypeMeta`와 `ListMeta`가 정의되어 있다. 이것은 versioned API 객체가 임베딩하는 메타데이터 타입이다:

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go (줄 42-57)
type TypeMeta struct {
    Kind       string `json:"kind,omitempty" protobuf:"bytes,1,opt,name=kind"`
    APIVersion string `json:"apiVersion,omitempty" protobuf:"bytes,2,opt,name=apiVersion"`
}

// 파일: staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go (줄 61-95)
type ListMeta struct {
    SelfLink           string `json:"selfLink,omitempty" protobuf:"bytes,1,opt,name=selfLink"`
    ResourceVersion    string `json:"resourceVersion,omitempty" protobuf:"bytes,2,opt,name=resourceVersion"`
    Continue           string `json:"continue,omitempty" protobuf:"bytes,3,opt,name=continue"`
    RemainingItemCount *int64 `json:"remainingItemCount,omitempty" protobuf:"bytes,4,opt,name=remainingItemCount"`
}
```

### 2.6 runtime.Object 계층도

```
                    runtime.Object
                         |
            +------------+-------------+
            |                          |
     구조화된 타입             Unstructured
     (Typed)                (비구조화)
            |                          |
   +--------+--------+       map[string]interface{}
   |        |        |
  Pod    Service   Deployment
   |
   +-- metav1.TypeMeta    (ObjectKind 구현)
   +-- metav1.ObjectMeta  (Name, Namespace, Labels 등)
   +-- Spec
   +-- Status
```

**왜 Unstructured가 별도로 존재하는가?** CRD(Custom Resource Definition)처럼 컴파일 타임에 Go 타입이 존재하지 않는 리소스를 다뤄야 하기 때문이다. Unstructured는 `map[string]interface{}`로 임의의 JSON 구조를 표현하면서도 `runtime.Object`를 구현한다.

---

## 3. Scheme (타입 레지스트리)

### 3.1 Scheme이 해결하는 문제

직렬화 프레임워크가 JSON `{"apiVersion":"v1","kind":"Pod",...}`를 받았을 때, 이를 어떤 Go 타입(`*core.Pod`)으로 역직렬화해야 하는지 알아야 한다. 반대로 `*core.Pod` 객체를 직렬화할 때 어떤 GVK를 기록해야 하는지도 알아야 한다. **Scheme은 GVK와 Go 타입 사이의 양방향 매핑 테이블**이다.

### 3.2 Scheme 구조체

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 50-95)
type Scheme struct {
    // GVK -> Go 타입 매핑 (역직렬화용)
    gvkToType map[schema.GroupVersionKind]reflect.Type

    // Go 타입 -> GVK 매핑 (직렬화용)
    typeToGVK map[reflect.Type][]schema.GroupVersionKind

    // 버전 없는 타입 (Status, APIVersions 등)
    unversionedTypes map[reflect.Type]schema.GroupVersionKind
    unversionedKinds map[string]reflect.Type

    // 필드 라벨 변환 함수
    fieldLabelConversionFuncs map[schema.GroupVersionKind]FieldLabelConversionFunc

    // 기본값 설정 함수
    defaulterFuncs map[reflect.Type]func(interface{})

    // 유효성 검증 함수
    validationFuncs map[reflect.Type]func(ctx context.Context, ...) field.ErrorList

    // 타입 변환기
    converter *conversion.Converter

    // 버전 우선순위
    versionPriority map[string][]string

    // 등록 순서 추적
    observedVersions []schema.GroupVersion

    // 스키마 이름 (디버깅용)
    schemeName string
}
```

**왜 이 모든 것이 하나의 구조체에 있는가?** Scheme은 **타입 레지스트리**이자 **변환 허브**이자 **기본값 적용기**이자 **유효성 검사기**를 겸한다. 이렇게 한 곳에 모으면 "이 타입에 대해 알려진 모든 메타데이터"를 한 번에 조회할 수 있다.

### 3.3 Scheme 생성

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 101-119)
func NewScheme() *Scheme {
    s := &Scheme{
        gvkToType:                 map[schema.GroupVersionKind]reflect.Type{},
        typeToGVK:                 map[reflect.Type][]schema.GroupVersionKind{},
        unversionedTypes:          map[reflect.Type]schema.GroupVersionKind{},
        unversionedKinds:          map[string]reflect.Type{},
        fieldLabelConversionFuncs: map[schema.GroupVersionKind]FieldLabelConversionFunc{},
        defaulterFuncs:            map[reflect.Type]func(interface{}){},
        versionPriority:           map[string][]string{},
        schemeName:                naming.GetNameFromCallsite(internalPackages...),
    }
    s.converter = conversion.NewConverter(nil)

    // 기본 변환 함수 등록
    utilruntime.Must(RegisterEmbeddedConversions(s))
    utilruntime.Must(RegisterStringConversions(s))
    return s
}
```

`schemeName`은 `naming.GetNameFromCallsite()`으로 호출 스택에서 자동 추출된다. 에러 메시지에 "어느 Scheme에서 문제가 발생했는지" 표시하기 위한 것이다.

### 3.4 타입 등록 (AddKnownTypes)

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 151-161)
func (s *Scheme) AddKnownTypes(gv schema.GroupVersion, types ...Object) {
    s.addObservedVersion(gv)
    for _, obj := range types {
        t := reflect.TypeOf(obj)
        if t.Kind() != reflect.Pointer {
            panic("All types must be pointers to structs.")
        }
        t = t.Elem()
        s.AddKnownTypeWithName(gv.WithKind(t.Name()), obj)
    }
}
```

**핵심 설계 결정**: Go 구조체의 이름이 곧 Kind가 된다. `*core.Pod`를 등록하면 Kind는 `"Pod"`가 된다. 이를 통해 코드의 타입명과 API의 Kind가 자동으로 일치한다.

### 3.5 AddKnownTypeWithName - 양방향 매핑

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 167-206)
func (s *Scheme) AddKnownTypeWithName(gvk schema.GroupVersionKind, obj Object) {
    s.addObservedVersion(gvk.GroupVersion())
    t := reflect.TypeOf(obj)
    // ... 유효성 검사 ...

    // GVK -> Type (역직렬화용)
    s.gvkToType[gvk] = t

    // Type -> GVK (직렬화용, 하나의 타입이 여러 GVK를 가질 수 있음)
    s.typeToGVK[t] = append(s.typeToGVK[t], gvk)

    // DeepCopyInto가 있으면 자기 자신으로의 변환을 자동 등록
    if m := reflect.ValueOf(obj).MethodByName("DeepCopyInto"); m.IsValid() ... {
        s.AddGeneratedConversionFunc(obj, obj, func(a, b interface{}, ...) error {
            reflect.ValueOf(a).MethodByName("DeepCopyInto").Call(...)
            b.(Object).GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
            return nil
        })
    }
}
```

**왜 DeepCopyInto 자동 등록이 중요한가?** 같은 타입 간의 변환(예: v1.Pod -> v1.Pod)은 단순 복사다. 이를 자동으로 등록하여 변환 프레임워크가 항상 일관되게 동작하도록 한다.

### 3.6 ObjectKinds - Go 타입에서 GVK 역조회

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 254-281)
func (s *Scheme) ObjectKinds(obj Object) ([]schema.GroupVersionKind, bool, error) {
    // Unstructured는 자체 GVK를 보고
    if _, ok := obj.(Unstructured); ok {
        gvk := obj.GetObjectKind().GroupVersionKind()
        // Kind와 Version이 설정되어 있어야 함
        return []schema.GroupVersionKind{gvk}, false, nil
    }

    // 구조화된 타입은 reflect.Type으로 조회
    v, err := conversion.EnforcePtr(obj)
    t := v.Type()

    gvks, ok := s.typeToGVK[t]
    if !ok {
        return nil, false, NewNotRegisteredErrForType(s.schemeName, t)
    }
    _, unversionedType := s.unversionedTypes[t]

    return gvks, unversionedType, nil
}
```

**왜 Unstructured와 구조화된 타입을 다르게 처리하는가?** 구조화된 타입은 Go 타입 자체가 GVK를 결정한다. 하지만 Unstructured는 `map[string]interface{}`이므로 Go 타입이 모든 리소스에 대해 동일하다. 따라서 객체 내부의 `apiVersion`/`kind` 필드 값으로 GVK를 판단해야 한다.

### 3.7 Defaulting (기본값 설정)

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 350-359)
func (s *Scheme) AddTypeDefaultingFunc(srcType Object, fn func(interface{})) {
    s.defaulterFuncs[reflect.TypeOf(srcType)] = fn
}

func (s *Scheme) Default(src Object) {
    if fn, ok := s.defaulterFuncs[reflect.TypeOf(src)]; ok {
        fn(src)
    }
}
```

**왜 Defaulting이 Scheme에 있는가?** 역직렬화 후 변환 전에 기본값을 채워야 한다. 예를 들어, Pod의 `restartPolicy`가 비어 있으면 `"Always"`로 설정해야 한다. 이 로직이 Scheme에 등록되어 있으면 코덱이 자동으로 적용할 수 있다.

### 3.8 Conversion (타입 변환)

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 396-460)
func (s *Scheme) Convert(in, out interface{}, context interface{}) error {
    unstructuredIn, okIn := in.(Unstructured)
    unstructuredOut, okOut := out.(Unstructured)
    switch {
    case okIn && okOut:
        // Unstructured -> Unstructured: 참조 복사
        unstructuredOut.SetUnstructuredContent(unstructuredIn.UnstructuredContent())
    case okOut:
        // Typed -> Unstructured: DefaultUnstructuredConverter 사용
        // internal 타입이면 먼저 external로 변환
    case okIn:
        // Unstructured -> Typed: 먼저 typed로 변환 후 일반 변환
    }
    // 일반 케이스: converter.Convert() 호출
    return s.converter.Convert(in, out, meta)
}
```

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go (줄 477-479)
func (s *Scheme) ConvertToVersion(in Object, target GroupVersioner) (Object, error) {
    return s.convertToVersion(true, in, target)
}
```

`ConvertToVersion`은 입력 객체를 지정된 버전으로 변환한다. 내부적으로 `convertToVersion()`이 다음 순서로 동작한다:

```
convertToVersion 흐름:
1. Unstructured이면 먼저 typed로 변환
2. reflect.Type으로 현재 GVK 조회
3. target.KindForGroupVersionKinds()로 대상 GVK 결정
4. 이미 대상 GVK면 복사만 하고 리턴 (변환 불필요)
5. unversioned 타입이면 복사만 하고 리턴
6. s.New(gvk)로 대상 타입의 빈 객체 생성
7. s.converter.Convert(in, out, meta)로 실제 변환
8. setTargetKind(out, gvk)로 GVK 설정
```

---

## 4. API Versioning

### 4.1 왜 API Versioning이 필요한가

Kubernetes API는 지속적으로 진화한다. 예를 들어:
- `extensions/v1beta1/Deployment` -> `apps/v1beta1/Deployment` -> `apps/v1/Deployment`
- 필드가 추가되고, 이름이 바뀌고, 타입이 변경된다

그런데 etcd에는 **하나의 버전**으로만 저장해야 한다. 클라이언트는 **자기가 원하는 버전**으로 요청한다. 이 간극을 메우는 것이 API Versioning이다.

### 4.2 Internal Type vs Versioned Type

```
+-----------------------------------------------------------+
|  Versioned Types (외부 표현)                                |
|                                                           |
|  +------------------+  +------------------+               |
|  | apps/v1beta1.    |  | apps/v1.         |               |
|  | Deployment       |  | Deployment       |               |
|  | (JSON/YAML 직렬화)|  | (JSON/YAML 직렬화)|               |
|  +--------+---------+  +--------+---------+               |
|           |                      |                        |
|           |   변환(Conversion)    |                        |
|           |                      |                        |
|           v                      v                        |
|  +--------------------------------------------------+    |
|  | Internal Type (내부 표현)                          |    |
|  |                                                   |    |
|  | apps.__internal__.Deployment                      |    |
|  | - 모든 필드의 합집합(superset)                      |    |
|  | - 직렬화하지 않음                                   |    |
|  | - 비즈니스 로직이 이 타입으로 작성됨                  |    |
|  +--------------------------------------------------+    |
+-----------------------------------------------------------+
```

**왜 Internal Type이 존재하는가?**

1. **비즈니스 로직 단순화**: 밸리데이션, 기본값 설정 등의 로직을 하나의 타입에 대해서만 작성하면 된다
2. **필드 변환 격리**: v1beta1에서 `spec.replicas`가 `*int32`이고 v1에서 `int32`라면, internal type에서 한 번만 정의하면 된다
3. **새 버전 추가 용이**: internal type과의 변환만 작성하면 모든 기존 버전과 호환된다

### 4.3 Hub-and-Spoke 변환 모델

```
         v1alpha1 ----+
                      |
         v1beta1 -----+----> Internal Type (Hub) <----+--- v1
                      |                                |
         v1beta2 ----+                                +--- v2
```

모든 버전 간 변환은 Internal Type(Hub)을 경유한다. N개의 버전이 있을 때:
- 직접 변환: N*(N-1)개의 변환 함수 필요
- Hub-and-Spoke: 2*N개의 변환 함수만 필요

이것이 Kubernetes가 Hub-and-Spoke 패턴을 선택한 이유다.

### 4.4 GroupVersionKind (GVK) 타입 시스템

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/schema/group_version.go (줄 142-146)
type GroupVersionKind struct {
    Group   string
    Version string
    Kind    string
}

// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/schema/group_version.go (줄 166-169)
type GroupVersion struct {
    Group   string
    Version string
}

// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/schema/group_version.go (줄 96-100)
type GroupVersionResource struct {
    Group    string
    Version  string
    Resource string
}
```

Kubernetes 타입 시스템의 3대 좌표:

| 좌표 | 예시 | 용도 |
|------|------|------|
| `GroupVersionKind` | `apps/v1/Deployment` | 객체의 타입 식별 (직렬화/역직렬화) |
| `GroupVersionResource` | `apps/v1/deployments` | REST 경로 식별 (URL 라우팅) |
| `GroupResource` | `apps/deployments` | 버전 무관 리소스 식별 |

**왜 Kind와 Resource가 분리되어 있는가?** Kind는 객체의 Go 타입명(단수, CamelCase: `Deployment`)이고, Resource는 REST 경로명(복수, lowercase: `deployments`)이다. 대부분 `strings.ToLower(Kind) + "s"`이지만, `Endpoints`처럼 예외가 있어 분리된 매핑이 필요하다.

### 4.5 SchemeBuilder 패턴

각 API 그룹은 자신의 타입을 Scheme에 등록해야 한다. SchemeBuilder는 이 등록 함수들을 수집하고 나중에 한꺼번에 적용하는 패턴이다.

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/scheme_builder.go (줄 23-48)
type SchemeBuilder []func(*Scheme) error

func (sb *SchemeBuilder) AddToScheme(s *Scheme) error {
    for _, f := range *sb {
        if err := f(s); err != nil {
            return err
        }
    }
    return nil
}

func (sb *SchemeBuilder) Register(funcs ...func(*Scheme) error) {
    for _, f := range funcs {
        *sb = append(*sb, f)
    }
}

func NewSchemeBuilder(funcs ...func(*Scheme) error) SchemeBuilder {
    var sb SchemeBuilder
    sb.Register(funcs...)
    return sb
}
```

**왜 SchemeBuilder가 필요한가?** code generation으로 생성된 변환/기본값 함수를 등록하기 위함이다. 생성된 코드의 `init()` 함수에서 `SchemeBuilder.Register()`를 호출하면, 나중에 `AddToScheme()`으로 한꺼번에 Scheme에 적용된다. 컴파일 타임에 생성된 타입을 명시적으로 참조하지 않아도 된다.

### 4.6 Core API 등록 예시

```go
// 파일: pkg/apis/core/register.go (줄 41-48)
var (
    SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
    AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
    scheme.AddKnownTypes(SchemeGroupVersion,
        &Pod{},
        &PodList{},
        &Service{},
        &ServiceList{},
        &Node{},
        &NodeList{},
        // ... 수십 개의 타입
    )
    return nil
}
```

`SchemeGroupVersion`은 `schema.GroupVersion{Group: "", Version: runtime.APIVersionInternal}`이다. 즉, Core API의 internal 타입들을 빈 그룹과 `__internal__` 버전으로 등록한다.

### 4.7 Install 패턴

```go
// 파일: pkg/apis/core/install/install.go (줄 29-38)
func init() {
    Install(legacyscheme.Scheme)
}

func Install(scheme *runtime.Scheme) {
    utilruntime.Must(core.AddToScheme(scheme))       // internal 타입 등록
    utilruntime.Must(v1.AddToScheme(scheme))          // v1 versioned 타입 등록
    utilruntime.Must(scheme.SetVersionPriority(v1.SchemeGroupVersion))  // v1을 우선 버전으로
}
```

Install 패턴의 흐름:

```
+---------------------------------------------------+
| install/install.go                                |
|                                                   |
| 1. core.AddToScheme(scheme)                       |
|    -> internal Pod, Service 등 등록                |
|    -> internal 변환 함수 등록                       |
|                                                   |
| 2. v1.AddToScheme(scheme)                         |
|    -> v1.Pod, v1.Service 등 등록                   |
|    -> v1 <-> internal 변환 함수 등록                |
|    -> v1 기본값 함수 등록                           |
|                                                   |
| 3. scheme.SetVersionPriority(v1.SchemeGroupVersion)|
|    -> v1을 해당 그룹의 "선호 버전"으로 설정          |
+---------------------------------------------------+
```

**왜 init()에서 호출하는가?** Go의 `init()` 함수는 패키지가 임포트되면 자동 실행된다. `install` 패키지를 `_`(blank import)로 가져오기만 하면 해당 API 그룹의 모든 타입이 글로벌 Scheme에 등록된다. 이 패턴 덕분에 API 서버의 `main()` 함수에서 일일이 등록 코드를 작성하지 않아도 된다.

### 4.8 Versioning Codec

실제 네트워크/디스크 I/O에서 버전 변환을 수행하는 것은 `versioning.codec`이다:

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/runtime/serializer/versioning/versioning.go (줄 75-90)
type codec struct {
    encoder   runtime.Encoder      // 실제 JSON/YAML/Protobuf 인코더
    decoder   runtime.Decoder      // 실제 디코더
    convertor runtime.ObjectConvertor // Scheme 기반 변환기
    creater   runtime.ObjectCreater   // 빈 객체 생성기
    typer     runtime.ObjectTyper     // 타입 판별기
    defaulter runtime.ObjectDefaulter // 기본값 적용기

    encodeVersion runtime.GroupVersioner // 인코딩할 버전
    decodeVersion runtime.GroupVersioner // 디코딩 결과 버전

    identifier         runtime.Identifier
    originalSchemeName string
}
```

Decode 흐름:

```
  요청 바이트 (JSON)
       |
       v
  1. decoder.Decode()    -- 원시 역직렬화
       |
       v
  2. defaulter.Default() -- 기본값 적용
       |
       v
  3. convertor.Convert() -- 버전 변환 (예: v1 -> internal)
       |
       v
  runtime.Object (내부 표현)
```

Encode 흐름:

```
  runtime.Object (내부 표현)
       |
       v
  1. convertor.ConvertToVersion() -- 버전 변환 (예: internal -> v1)
       |
       v
  2. encoder.Encode()             -- 원시 직렬화
       |
       v
  응답 바이트 (JSON)
```

**왜 codec이 이렇게 많은 것을 조합하는가?** 직렬화, 기본값 설정, 변환은 서로 독립적인 관심사지만, API 요청 처리 시에는 반드시 정해진 순서로 실행되어야 한다. codec이 이 순서를 보장한다.

---

## 5. Strategic Merge Patch (SMP)

### 5.1 왜 Strategic Merge Patch인가

HTTP PATCH 요청으로 리소스를 부분 수정할 때 세 가지 전략이 있다:

| 방식 | 장점 | 단점 |
|------|------|------|
| JSON Patch (RFC 6902) | 정밀한 경로 기반 수정 | 복잡한 패치 문서, 순서 의존적 |
| JSON Merge Patch (RFC 7386) | 단순한 diff | 배열을 **통째로 교체**만 가능 |
| Strategic Merge Patch | 배열 필드별 merge/replace 선택 가능 | Kubernetes 전용 |

Kubernetes에서 JSON Merge Patch의 가장 큰 문제는 **배열 처리**다. 예를 들어 Pod의 `containers` 배열에 sidecar를 추가하려면, JSON Merge Patch로는 기존 컨테이너 전체를 다시 보내야 한다. Strategic Merge Patch는 배열 필드에 **merge key**(`name`)를 지정하여 원소 단위 병합을 지원한다.

### 5.2 Patch Directives

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go (줄 40-51)
const (
    directiveMarker  = "$patch"
    deleteDirective  = "delete"
    replaceDirective = "replace"
    mergeDirective   = "merge"

    retainKeysStrategy = "retainKeys"

    deleteFromPrimitiveListDirectivePrefix = "$deleteFromPrimitiveList"
    retainKeysDirective                    = "$" + retainKeysStrategy
    setElementOrderDirectivePrefix         = "$setElementOrder"
)
```

| Directive | 의미 | 사용 예 |
|-----------|------|---------|
| `$patch: delete` | 해당 맵을 삭제 | `{"$patch": "delete"}` |
| `$patch: replace` | 해당 맵을 전체 교체 (merge 대신) | `{"$patch": "replace", "key": "val"}` |
| `$patch: merge` | 해당 맵을 병합 (기본 동작) | `{"$patch": "merge", "key": "val"}` |
| `$deleteFromPrimitiveList/key` | 원시값 리스트에서 특정 값 삭제 | `{"$deleteFromPrimitiveList/args": ["-v"]}` |
| `$retainKeys` | 지정된 키만 유지, 나머지 삭제 | `{"$retainKeys": ["a","b"], "a": 1}` |
| `$setElementOrder/key` | 리스트 원소 순서 지정 | `{"$setElementOrder/containers": [{"name":"a"},{"name":"b"}]}` |

### 5.3 PatchMeta - 필드별 전략 정의

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/meta.go (줄 34-65)
type PatchMeta struct {
    patchStrategies []string
    patchMergeKey   string
}

type LookupPatchMeta interface {
    LookupPatchMetadataForStruct(key string) (LookupPatchMeta, PatchMeta, error)
    LookupPatchMetadataForSlice(key string) (LookupPatchMeta, PatchMeta, error)
    Name() string
}
```

Go 구조체의 태그에서 전략 정보를 추출한다:

```go
// 예: Pod의 containers 필드
Containers []Container `json:"containers" patchStrategy:"merge" patchMergeKey:"name"`
```

이 태그는 다음을 의미한다:
- `patchStrategy:"merge"` - 배열을 통째로 교체하지 않고 원소 단위로 병합
- `patchMergeKey:"name"` - `name` 필드를 키로 사용하여 같은 원소를 식별

### 5.4 2-Way Merge Patch

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go (줄 94-101)
func CreateTwoWayMergePatch(original, modified []byte, dataStruct interface{},
    fns ...mergepatch.PreconditionFunc) ([]byte, error) {
    schema, err := NewPatchMetaFromStruct(dataStruct)
    if err != nil {
        return nil, err
    }
    return CreateTwoWayMergePatchUsingLookupPatchMeta(original, modified, schema, fns...)
}
```

2-way merge는 **original**과 **modified**만 비교한다:

```
Original          Modified           Patch
{                 {                  {
  "a": 1,           "a": 1,            "b": 3,
  "b": 2            "b": 3,            "c": 4
}                   "c": 4           }
                  }
```

### 5.5 3-Way Merge Patch

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go (줄 2101)
func CreateThreeWayMergePatch(original, modified, current []byte,
    schema LookupPatchMeta, overwrite bool,
    fns ...mergepatch.PreconditionFunc) ([]byte, error) {
```

3-way merge는 **original**(마지막으로 적용한 구성), **modified**(새로 적용할 구성), **current**(서버의 현재 상태) 세 가지를 비교한다:

```
                  original (Last Applied)
                 /                       \
                v                         v
    modified (Desired)              current (Live)
                \                       /
                 v                     v
               3-Way Merge Patch
```

**왜 3-way가 필요한가?** `kubectl apply` 시나리오를 생각해보자:

1. 사용자가 YAML(original)을 `kubectl apply`로 적용
2. 서버에서 기본값이 추가됨(current)
3. 사용자가 YAML을 수정(modified)하고 다시 `kubectl apply`

2-way로는 "서버가 추가한 기본값"과 "사용자가 의도적으로 삭제한 필드"를 구분할 수 없다. 3-way는 original을 기준으로 "modified에서 변경된 것"과 "current에서 변경된 것"을 분리하여 충돌을 감지한다.

### 5.6 StrategicMergePatch 적용

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go (줄 812)
func StrategicMergePatch(original, patch []byte, dataStruct interface{}) ([]byte, error) {
    schema, err := NewPatchMetaFromStruct(dataStruct)
    if err != nil {
        return nil, err
    }
    return StrategicMergePatchUsingLookupPatchMeta(original, patch, schema)
}
```

### 5.7 배열 Merge 전략 비교

```
# JSON Merge Patch (배열 전체 교체)
Original:  {"containers": [{"name":"app","image":"v1"}, {"name":"sidecar","image":"v1"}]}
Patch:     {"containers": [{"name":"app","image":"v2"}]}
Result:    {"containers": [{"name":"app","image":"v2"}]}  <-- sidecar가 사라짐!

# Strategic Merge Patch (merge key 기반 병합)
Original:  {"containers": [{"name":"app","image":"v1"}, {"name":"sidecar","image":"v1"}]}
Patch:     {"containers": [{"name":"app","image":"v2"}]}
Result:    {"containers": [{"name":"app","image":"v2"}, {"name":"sidecar","image":"v1"}]}
```

```
SMP 배열 처리 알고리즘:

1. patchStrategy 확인
   - "replace" → 배열 전체 교체
   - "merge"   → 원소 단위 병합 (아래 계속)

2. patchMergeKey로 원소 매칭
   - original[i].name == patch[j].name → 같은 원소
   - 매칭되지 않는 patch 원소 → 새로 추가

3. $deleteFromPrimitiveList 처리
   - 원시값 리스트에서 특정 값 제거

4. $setElementOrder 처리
   - 병합 후 최종 순서 적용
```

---

## 6. Server-Side Apply (SSA)

### 6.1 왜 Server-Side Apply인가

`kubectl apply`는 원래 **클라이언트 측**(Client-Side Apply)에서 3-way merge patch를 계산하여 서버로 보냈다. 이 방식의 문제:

1. **last-applied-configuration 주석**: 원본 구성을 `kubectl.kubernetes.io/last-applied-configuration` 주석에 저장 -> 객체가 비대해짐
2. **필드 소유권 부재**: 누가 어떤 필드를 "소유"하는지 추적 불가 -> 컨트롤러 간 충돌
3. **충돌 감지 부족**: 다른 도구(Helm, Terraform)로 같은 필드를 수정해도 감지 불가
4. **kubectl 의존성**: 3-way merge 로직이 kubectl에 하드코딩

Server-Side Apply(SSA, GA in v1.22)는 이 모든 문제를 서버 측에서 해결한다.

### 6.2 FieldManager

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/managedfields/fieldmanager.go (줄 30-32)
// FieldManager updates the managed fields and merges applied configurations.
type FieldManager = internal.FieldManager
```

실제 구현:

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/fieldmanager.go (줄 45-48)
type FieldManager struct {
    fieldManager Manager
    subresource  string
}
```

### 6.3 FieldManager 데코레이터 체인

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/fieldmanager.go (줄 57-75)
func NewDefaultFieldManager(f Manager, typeConverter TypeConverter,
    objectConverter runtime.ObjectConvertor, objectCreater runtime.ObjectCreater,
    kind schema.GroupVersionKind, subresource string) *FieldManager {
    return NewFieldManager(
        NewVersionCheckManager(
            NewLastAppliedUpdater(
                NewLastAppliedManager(
                    NewProbabilisticSkipNonAppliedManager(
                        NewCapManagersManager(
                            NewBuildManagerInfoManager(
                                NewManagedFieldsUpdater(
                                    NewStripMetaManager(f),
                                ), kind.GroupVersion(), subresource,
                            ), DefaultMaxUpdateManagers,
                        ), objectCreater, DefaultTrackOnCreateProbability,
                    ), typeConverter, objectConverter, kind.GroupVersion(),
                ),
            ), kind,
        ), subresource,
    )
}
```

이 중첩 구조는 **데코레이터 패턴**이다. 각 레이어가 하나의 관심사를 담당한다:

```
+------------------------------------------------------------------+
|  FieldManager (외부 인터페이스)                                     |
|  +--------------------------------------------------------------+ |
|  | VersionCheckManager - GVK 버전 검증                           | |
|  | +----------------------------------------------------------+ | |
|  | | LastAppliedUpdater - last-applied-configuration 주석 갱신  | | |
|  | | +------------------------------------------------------+ | | |
|  | | | LastAppliedManager - CSA 호환성 (주석 기반 추적)        | | | |
|  | | | +--------------------------------------------------+ | | | |
|  | | | | ProbabilisticSkipNonAppliedManager                | | | | |
|  | | | | - 비Apply 요청 스킵 최적화                          | | | | |
|  | | | | +----------------------------------------------+ | | | | |
|  | | | | | CapManagersManager                            | | | | | |
|  | | | | | - 매니저 수 제한 (과도한 엔트리 방지)             | | | | | |
|  | | | | | +------------------------------------------+ | | | | | |
|  | | | | | | BuildManagerInfoManager                   | | | | | | |
|  | | | | | | - 매니저 이름/버전 정보 빌드                 | | | | | | |
|  | | | | | | +--------------------------------------+ | | | | | | |
|  | | | | | | | ManagedFieldsUpdater                  | | | | | | | |
|  | | | | | | | - managedFields 실제 갱신              | | | | | | | |
|  | | | | | | | +----------------------------------+ | | | | | | | |
|  | | | | | | | | StripMetaManager                  | | | | | | | | |
|  | | | | | | | | - 메타데이터 필드 제거 (비교용)      | | | | | | | | |
|  | | | | | | | | +------------------------------+ | | | | | | | | |
|  | | | | | | | | | StructuredMergeManager (코어) | | | | | | | | | |
|  | | | | | | | | +------------------------------+ | | | | | | | | |
+--+--+--+--+--+--+--+------------------------------+-+-+-+-+-+-+-+-+
```

**왜 이렇게 많은 데코레이터가 필요한가?** 각 관심사를 독립적으로 테스트하고 교체할 수 있기 때문이다. 예를 들어 CRD용 FieldManager는 `StructuredMergeManager` 대신 `CRDStructuredMergeManager`를 사용하지만, 나머지 데코레이터는 동일하다.

### 6.4 Update 흐름 (일반 PUT/PATCH)

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/fieldmanager.go (줄 120-140)
func (f *FieldManager) Update(liveObj, newObj runtime.Object, manager string) (object runtime.Object, err error) {
    isSubresource := f.subresource != ""
    managed, err := decodeLiveOrNew(liveObj, newObj, isSubresource)
    if err != nil {
        return newObj, nil
    }

    RemoveObjectManagedFields(newObj)

    if object, managed, err = f.fieldManager.Update(liveObj, newObj, managed, manager); err != nil {
        return nil, err
    }

    if err = EncodeObjectManagedFields(object, managed); err != nil {
        return nil, fmt.Errorf("failed to encode managed fields: %v", err)
    }

    return object, nil
}
```

Update 흐름:

```
1. decodeLiveOrNew() - 기존 managedFields 디코딩
   - 서브리소스면 liveObj의 managedFields 사용
   - 빈 managedFields로 리셋 요청 감지

2. RemoveObjectManagedFields(newObj)
   - newObj에서 managedFields 제거 (비교 시 방해 방지)

3. f.fieldManager.Update()
   - 데코레이터 체인을 통해 실제 diff 계산
   - managed fields 갱신

4. EncodeObjectManagedFields()
   - 갱신된 managedFields를 객체에 다시 인코딩
```

### 6.5 Apply 흐름 (Server-Side Apply)

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/fieldmanager.go (줄 183-209)
func (f *FieldManager) Apply(liveObj, appliedObj runtime.Object, manager string, force bool) (object runtime.Object, err error) {
    accessor, err := meta.Accessor(liveObj)
    // ...
    managed, err := DecodeManagedFields(accessor.GetManagedFields())
    // ...
    object, managed, err = f.fieldManager.Apply(liveObj, appliedObj, managed, manager, force)
    if err != nil {
        if conflicts, ok := err.(merge.Conflicts); ok {
            return nil, NewConflictError(conflicts)
        }
        return nil, err
    }
    // ...
    return object, nil
}
```

Apply 흐름:

```
클라이언트 요청 (Apply PATCH)
       |
       v
1. liveObj의 managedFields 디코딩
       |
       v
2. appliedObj와 liveObj를 Structured Merge로 병합
       |
       v
3. 충돌 감지 (다른 매니저가 소유한 필드 변경 시도?)
       |
   +---+---+
   |       |
   v       v
 충돌 O  충돌 X
   |       |
   v       v
 force?  병합 결과 + 갱신된 managedFields 반환
   |
   +---+---+
   |       |
   v       v
 force=   force=
 true     false
   |       |
   v       v
 강제     409 Conflict
 적용     응답
```

### 6.6 ManagedFields 구조

SSA는 각 객체에 `metadata.managedFields`를 추가한다:

```yaml
metadata:
  managedFields:
  - manager: kubectl-client-side-apply
    operation: Apply
    apiVersion: apps/v1
    time: "2024-01-15T10:00:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:replicas: {}
        f:template:
          f:spec:
            f:containers:
              k:{"name":"nginx"}:
                f:image: {}
  - manager: hpa-controller
    operation: Update
    apiVersion: autoscaling/v1
    time: "2024-01-15T10:05:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:replicas: {}
```

위 예시에서 `spec.replicas`는 두 매니저가 모두 소유를 주장한다. 이때:
- `kubectl apply`로 replicas를 변경하면 충돌 발생
- `--force-conflicts` 옵션으로 소유권 강제 이전 가능

### 6.7 ExtractInto - Apply 구성 추출

```go
// 파일: staging/src/k8s.io/apimachinery/pkg/util/managedfields/extract.go (줄 53-89)
func ExtractInto(object runtime.Object, objectType typed.ParseableType,
    fieldManager string, applyConfiguration interface{}, subresource string) error {
    typedObj, err := toTyped(object, objectType)
    // ...
    fieldsEntry, ok := findManagedFields(accessor, fieldManager, subresource)
    if !ok {
        return nil  // 해당 매니저의 필드가 없으면 빈 상태로 반환
    }
    fieldset := &fieldpath.Set{}
    err = fieldset.FromJSON(bytes.NewReader(fieldsEntry.FieldsV1.Raw))
    // ...
    u := typedObj.ExtractItems(fieldset.Leaves()).AsValue().Unstructured()
    // ...
    return runtime.DefaultUnstructuredConverter.FromUnstructured(m, applyConfiguration)
}
```

**왜 ExtractInto가 중요한가?** "Read-Modify-Write" 패턴을 SSA에서 안전하게 구현할 수 있다:

```
1. ExtractInto()로 내가 소유한 필드만 추출
2. 추출된 ApplyConfiguration 수정
3. Apply()로 서버에 전송
→ 다른 매니저의 필드는 건드리지 않음
```

### 6.8 CSA vs SSA 비교

```
+-----------------------------+----------------------------------+
|   Client-Side Apply (CSA)   |    Server-Side Apply (SSA)       |
+-----------------------------+----------------------------------+
| kubectl이 3-way merge 계산  | API 서버가 structured merge 수행 |
| last-applied 주석에 원본 저장| managedFields에 소유권 추적      |
| 충돌 감지 불가               | 필드 수준 충돌 감지              |
| kubectl 전용                 | 모든 클라이언트 사용 가능        |
| Content-Type: SMP           | Content-Type:                    |
|                             |   application/apply-patch+yaml   |
+-----------------------------+----------------------------------+
```

---

## 7. API Aggregation Layer

### 7.1 왜 API Aggregation이 필요한가

CRD만으로는 부족한 경우가 있다:
- **커스텀 서브리소스** (예: `/scale`, `/status` 외의 서브리소스)
- **커스텀 스토리지 백엔드** (etcd 대신 다른 DB)
- **프로토콜 변환** (Kubernetes API를 비-Kubernetes 시스템에 매핑)
- **고도의 커스텀 로직** (밸리데이션, 변환이 웹훅으로 부족한 경우)

API Aggregation은 별도의 API 서버를 작성하여 kube-apiserver 뒤에 등록하는 메커니즘이다.

### 7.2 APIService 리소스

```go
// 파일: staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/types.go (줄 44-86)
type APIServiceSpec struct {
    Service *ServiceReference       // 백엔드 서비스 참조 (nil이면 로컬)
    Group   string                  // API 그룹명
    Version string                  // API 버전
    InsecureSkipTLSVerify bool      // TLS 검증 스킵 (비권장)
    CABundle []byte                 // 서버 인증서 CA
    GroupPriorityMinimum int32      // 그룹 우선순위
    VersionPriority int32           // 버전 우선순위
}

type ServiceReference struct {
    Namespace string
    Name      string
    Port      int32
}
```

APIService YAML 예시:

```yaml
apiVersion: apiregistration.k8s.io/v1
kind: APIService
metadata:
  name: v1beta1.metrics.k8s.io
spec:
  service:
    name: metrics-server
    namespace: kube-system
  group: metrics.k8s.io
  version: v1beta1
  groupPriorityMinimum: 100
  versionPriority: 100
  caBundle: <base64-encoded-ca>
```

### 7.3 kube-aggregator 구성

```go
// 파일: staging/src/k8s.io/kube-aggregator/pkg/apiserver/apiserver.go (줄 86-116)
type ExtraConfig struct {
    PeerAdvertiseAddress peerreconcilers.PeerAdvertiseAddress
    ProxyClientCertFile string      // 프록시 인증서
    ProxyClientKeyFile  string      // 프록시 키
    ProxyTransport      *http.Transport
    ServiceResolver     ServiceResolver  // 서비스 -> 엔드포인트 해석
    RejectForwardingRedirects bool
    PeerProxy utilpeerproxy.Interface
}
```

### 7.4 프록시 메커니즘

```
클라이언트 요청
    |
    v
+-------------------+
| kube-aggregator   |
| (kube-apiserver   |
|  내장)            |
+--------+----------+
         |
         | 1. URL 경로에서 API Group/Version 추출
         |    /apis/metrics.k8s.io/v1beta1/nodes
         |
         | 2. APIService 조회
         |    -> spec.service가 nil이면 로컬 처리
         |    -> spec.service가 있으면 프록시
         |
         v
+---+----+----+---+
|   |         |   |
|   v         v   |
| 로컬      프록시 |
| 처리      전달  |
|   |         |   |
|   v         v   |
| kube-    metrics|
| apiserver server|
| 내장 API        |
+---+----+----+---+

프록시 전달 시:
- ProxyClientCert로 인증 (aggregator -> extension apiserver)
- CABundle로 서버 인증서 검증
- X-Forwarded-For 헤더 추가
- 요청/응답 스트리밍 지원 (WebSocket, Watch)
```

### 7.5 Local vs Remote APIService

| 속성 | Local APIService | Remote APIService |
|------|-----------------|-------------------|
| `spec.service` | nil | 서비스 참조 |
| 예시 | `v1.`, `apps/v1` | `metrics.k8s.io/v1beta1` |
| 처리 | kube-apiserver 내장 핸들러 | 프록시를 통한 외부 서버 |
| 가용성 체크 | 항상 Available | 서비스 엔드포인트 확인 |

### 7.6 API Aggregation 아키텍처

```
+------------------------------------------------------+
|                   kube-apiserver                      |
|                                                      |
| +-----------+  +-----------+  +-------------------+  |
| | Core APIs |  | Built-in  |  | kube-aggregator   |  |
| | (v1)      |  | APIs      |  |                   |  |
| |           |  | (apps/v1) |  | APIService 관리    |  |
| |           |  |           |  | 프록시 라우팅       |  |
| +-----------+  +-----------+  | Available 상태 관리 |  |
|                               +--------+----------+  |
|                                        |             |
+----------------------------------------|-------------+
                                         |
                    +--------------------+--------------------+
                    |                    |                    |
                    v                    v                    v
            +-----------+       +-----------+       +-----------+
            | metrics-  |       | custom-   |       | service-  |
            | server    |       | api-      |       | catalog   |
            |           |       | server    |       |           |
            | metrics.  |       | custom.   |       | service   |
            | k8s.io    |       | example.  |       | catalog.  |
            |           |       | com       |       | k8s.io    |
            +-----------+       +-----------+       +-----------+
```

---

## 8. API Discovery & OpenAPI

### 8.1 API Discovery

API Discovery는 클라이언트가 "이 서버가 어떤 API를 제공하는가?"를 동적으로 파악하는 메커니즘이다.

```go
// 파일: staging/src/k8s.io/client-go/discovery/discovery_client.go (줄 79-90)
type DiscoveryInterface interface {
    RESTClient() restclient.Interface
    ServerGroupsInterface       // /apis, /api 엔드포인트
    ServerResourcesInterface    // /apis/<group>/<version> 리소스 목록
    ServerVersionInterface      // /version 서버 버전
    OpenAPISchemaInterface      // /openapi/v2 스키마
    OpenAPIV3SchemaInterface    // /openapi/v3 스키마
    WithLegacy() DiscoveryInterface  // 레거시 디스커버리 형식
}
```

Discovery API 엔드포인트:

```
GET /api                         -> APIVersions (core 그룹의 버전 목록)
GET /api/v1                      -> APIResourceList (v1 리소스 목록)
GET /apis                        -> APIGroupList (모든 API 그룹)
GET /apis/apps                   -> APIGroup (apps 그룹 정보)
GET /apis/apps/v1                -> APIResourceList (apps/v1 리소스 목록)
GET /version                     -> Info (서버 버전)
GET /openapi/v2                  -> OpenAPI v2 스키마
GET /openapi/v3                  -> OpenAPI v3 Discovery (경로 목록)
GET /openapi/v3/apis/apps/v1     -> OpenAPI v3 스키마 (특정 그룹/버전)
```

### 8.2 Aggregated Discovery

기존 Discovery는 각 API 그룹/버전마다 별도의 HTTP 요청이 필요했다. 수십 개의 그룹이 있으면 수십 번의 HTTP 왕복이 발생한다. Aggregated Discovery(`APIGroupDiscoveryList`)는 한 번의 요청으로 모든 그룹과 리소스를 가져온다:

```go
// 파일: staging/src/k8s.io/client-go/discovery/discovery_client.go (줄 74-75)
var v2Beta1GVK = schema.GroupVersionKind{
    Group: "apidiscovery.k8s.io", Version: "v2beta1", Kind: "APIGroupDiscoveryList"}
var v2GVK = schema.GroupVersionKind{
    Group: "apidiscovery.k8s.io", Version: "v2", Kind: "APIGroupDiscoveryList"}
```

```
기존 Discovery:
  GET /apis                          1번 요청
  GET /apis/apps/v1                  2번 요청
  GET /apis/batch/v1                 3번 요청
  GET /apis/networking.k8s.io/v1     4번 요청
  ... (N개의 그룹 * M개의 버전)

Aggregated Discovery:
  GET /apis                          1번 요청으로 전부 수신
  Accept: application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList
```

### 8.3 OpenAPI 클라이언트

```go
// 파일: staging/src/k8s.io/client-go/openapi/client.go (줄 28-73)
type Client interface {
    Paths() (map[string]GroupVersion, error)
}

type client struct {
    restClient rest.Interface
}

func (c *client) Paths() (map[string]GroupVersion, error) {
    data, err := c.restClient.Get().
        AbsPath("/openapi/v3").
        Do(context.TODO()).
        Raw()
    // ...
    discoMap := &handler3.OpenAPIV3Discovery{}
    err = json.Unmarshal(data, discoMap)
    // ...
    result := map[string]GroupVersion{}
    for k, v := range discoMap.Paths {
        result[k] = newGroupVersion(c, v, useClientPrefix)
    }
    return result, nil
}
```

OpenAPI v3 Discovery 응답 구조:

```json
{
  "paths": {
    "api/v1": {
      "serverRelativeURL": "/openapi/v3/api/v1?hash=abc123"
    },
    "apis/apps/v1": {
      "serverRelativeURL": "/openapi/v3/apis/apps/v1?hash=def456"
    }
  }
}
```

`hash` 쿼리 파라미터는 **캐시 무효화**를 위한 것이다. 스키마가 변경되면 해시가 바뀌므로 클라이언트는 해시가 같으면 캐시를 사용할 수 있다.

### 8.4 Discovery의 활용처

| 사용자 | 활용 방식 |
|--------|----------|
| `kubectl` | 리소스 축약어 해석 (`po` -> `pods`), API 존재 여부 확인 |
| `kubectl api-resources` | 모든 리소스 유형 목록 출력 |
| `kubectl explain` | OpenAPI 스키마에서 필드 설명 추출 |
| client-go | RESTMapper 구성 (GVR -> GVK 매핑) |
| Helm | 템플릿 렌더링 시 API 가용성 확인 |
| Operator SDK | 지원하는 API 버전 자동 감지 |

### 8.5 RESTMapper

Discovery 결과를 바탕으로 `RESTMapper`가 구성된다. RESTMapper는 GVK(타입)와 GVR(REST 경로) 사이를 매핑한다:

```
RESTMapper 매핑 예시:

  GVK: apps/v1/Deployment
  GVR: apps/v1/deployments
  Scope: Namespaced

  GVK: /v1/Node
  GVR: /v1/nodes
  Scope: Cluster

  GVK: /v1/Pod
  GVR: /v1/pods
  Scope: Namespaced
```

---

## 9. 소스코드 맵

```
staging/src/k8s.io/apimachinery/
├── pkg/
│   ├── runtime/
│   │   ├── interfaces.go          # runtime.Object 인터페이스 (줄 337-340)
│   │   ├── types.go               # TypeMeta 구조체 (줄 38-43)
│   │   ├── register.go            # TypeMeta의 ObjectKind 구현 (줄 21-31)
│   │   ├── scheme.go              # Scheme 구조체 (줄 50-95)
│   │   │                          # NewScheme (줄 101-119)
│   │   │                          # AddKnownTypes (줄 151-161)
│   │   │                          # AddKnownTypeWithName (줄 167-206)
│   │   │                          # ObjectKinds (줄 254-281)
│   │   │                          # Default (줄 355-359)
│   │   │                          # Convert (줄 396-460)
│   │   │                          # ConvertToVersion (줄 477-479)
│   │   ├── scheme_builder.go      # SchemeBuilder 패턴 (줄 23-48)
│   │   └── schema/
│   │       └── group_version.go   # GVK, GVR, GroupVersion (줄 142-169)
│   ├── apis/meta/v1/
│   │   └── types.go               # TypeMeta (줄 42-57), ListMeta (줄 61-95)
│   └── util/
│       ├── strategicpatch/
│       │   ├── patch.go           # SMP 핵심 (줄 40-51: directives)
│       │   │                      # CreateTwoWayMergePatch (줄 94-100)
│       │   │                      # StrategicMergePatch (줄 812)
│       │   │                      # CreateThreeWayMergePatch (줄 2101)
│       │   └── meta.go            # PatchMeta (줄 34-65)
│       └── managedfields/
│           ├── fieldmanager.go    # FieldManager 공개 API (줄 30-53)
│           ├── extract.go         # ExtractInto (줄 37-88)
│           └── internal/
│               └── fieldmanager.go # 내부 FieldManager (줄 45-75)
│                                   # Update (줄 120-140)
│                                   # Apply (줄 183-209)

staging/src/k8s.io/apimachinery/pkg/runtime/serializer/versioning/
└── versioning.go                  # Versioning codec (줄 57-90)

staging/src/k8s.io/kube-aggregator/pkg/
├── apiserver/
│   └── apiserver.go               # ExtraConfig (줄 86-116)
└── apis/apiregistration/
    └── types.go                   # APIServiceSpec (줄 44-86)

staging/src/k8s.io/client-go/
├── discovery/
│   └── discovery_client.go        # DiscoveryInterface (줄 79-90)
└── openapi/
    └── client.go                  # OpenAPI Client (줄 28-73)

pkg/apis/core/
├── register.go                    # Core API 타입 등록 (줄 41-48)
└── install/
    └── install.go                 # Install 패턴 (줄 34-38)
```

---

## 10. 핵심 정리

### 10.1 API Machinery의 핵심 설계 원칙

```
+---------------------------------------------------------------+
|  설계 원칙             | 구현 메커니즘                          |
+------------------------+---------------------------------------+
| 타입 통일              | runtime.Object 인터페이스             |
| 양방향 타입 매핑        | Scheme (gvkToType / typeToGVK)       |
| N:M 버전 변환          | Hub-and-Spoke (Internal Type)        |
| 선언적 패치            | Strategic Merge Patch + Directives   |
| 필드 소유권 추적        | Server-Side Apply + ManagedFields    |
| 런타임 확장            | API Aggregation (APIService 프록시)  |
| 동적 타입 발견          | Discovery API + OpenAPI              |
+------------------------+---------------------------------------+
```

### 10.2 왜 이 설계인가 - 근본적인 이유

1. **runtime.Object가 메서드를 2개만 요구하는 이유**: 직렬화(ObjectKind)와 불변성(DeepCopy)만 보장하면 나머지는 Scheme이 리플렉션으로 처리할 수 있다. 인터페이스를 최소화하여 새 타입 추가 비용을 낮춘다.

2. **Scheme이 글로벌 레지스트리인 이유**: API 서버 시작 시 모든 타입이 등록되고, 이후에는 변경되지 않는다(`Schemes are not expected to change at runtime`). 이 불변성이 동시성 안전성을 보장한다.

3. **Internal Type이 존재하는 이유**: N개의 버전이 있을 때 O(N^2) 변환 함수 대신 O(2N)으로 줄이고, 비즈니스 로직을 하나의 타입에만 작성할 수 있다.

4. **Strategic Merge Patch가 Kubernetes 전용인 이유**: 표준 JSON Merge Patch는 배열을 통째로 교체한다. Kubernetes의 배열(containers, volumes 등)은 원소 단위 병합이 필수적이어서, Go 구조체 태그 기반의 merge 전략이 필요했다.

5. **Server-Side Apply가 도입된 이유**: Client-Side Apply의 `last-applied-configuration` 주석은 확장성 한계가 있고, 여러 도구 간 충돌을 감지할 수 없다. 서버가 필드 소유권을 추적하면 이 문제를 근본적으로 해결할 수 있다.

6. **API Aggregation이 CRD와 공존하는 이유**: CRD는 "선언적 API 확장"(etcd에 CRUD, 웹훅으로 로직), API Aggregation은 "프로그래밍 방식 API 확장"(독립 서버, 커스텀 스토리지). 유즈케이스가 다르다.

### 10.3 데이터 흐름 종합

```
클라이언트 요청 (kubectl apply -f deployment.yaml)
       |
       v
[1] HTTP 요청 수신 (Content-Type: application/apply-patch+yaml)
       |
       v
[2] 인증/인가 (authn/authz)
       |
       v
[3] API Discovery로 GVR 확인
    /apis/apps/v1/namespaces/default/deployments/nginx
       |
       v
[4] RESTMapper: GVR -> GVK (apps/v1/deployments -> apps/v1/Deployment)
       |
       v
[5] Versioning Codec Decode
    5a. JSON 역직렬화 -> apps/v1.Deployment
    5b. Defaulting (기본값 적용)
    5c. Conversion (v1 -> internal)
       |
       v
[6] Admission (밸리데이션, 뮤테이팅)
       |
       v
[7] FieldManager.Apply()
    7a. Structured Merge (appliedObj + liveObj)
    7b. 충돌 감지 (managedFields 기반)
    7c. managedFields 갱신
       |
       v
[8] Versioning Codec Encode
    8a. Conversion (internal -> storage version)
    8b. JSON 직렬화
       |
       v
[9] etcd 저장
       |
       v
[10] 응답 Encode
    10a. Conversion (internal -> 요청 버전)
    10b. JSON 직렬화
       |
       v
클라이언트 응답
```

### 10.4 API Machinery가 없었다면?

| 문제 | API Machinery 없이 | API Machinery 사용 |
|------|-------------------|-------------------|
| 새 API 버전 추가 | 모든 핸들러에 switch 분기 | 변환 함수 2개만 추가 |
| 새 API 그룹 추가 | 코드 전체 수정 | install 패키지 import만 추가 |
| 부분 업데이트 | 전체 객체 덮어쓰기 | SMP/SSA로 필드 단위 패치 |
| 멀티 컨트롤러 충돌 | 마지막 쓰기가 승리 | managedFields로 충돌 감지 |
| API 확장 | kube-apiserver 코드 수정 | CRD or APIService 등록 |
| 클라이언트 호환성 | 서버 API 변경 시 클라이언트 깨짐 | Discovery로 동적 적응 |

API Machinery는 Kubernetes가 수년간 하위 호환성을 유지하면서도 빠르게 진화할 수 있는 핵심 인프라다. 단순한 유틸리티 라이브러리가 아니라, Kubernetes API의 모든 "규칙"을 코드로 구현한 **계약 프레임워크**다.
