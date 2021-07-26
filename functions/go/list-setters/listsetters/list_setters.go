package listsetters

import (
	goerrors "errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	kptv1 "github.com/GoogleContainerTools/kpt-functions-catalog/functions/go/fix/v1"
	"sigs.k8s.io/kustomize/kyaml/errors"
	"sigs.k8s.io/kustomize/kyaml/kio/kioutil"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

const SetterCommentIdentifier = "# kpt-set: "

// ListSetters lists setters identified by the setter comments
type ListSetters struct {
	// ScalarSetters holds the discovered scalar setters
	ScalarSetters map[string]*ScalarSetter

	// ArraySetters holds the discovered array setters
	ArraySetters map[string]*ArraySetter

	// Warnings holds recoverable error info that occurred during setter discovery
	Warnings []*ErrSetterDiscovery

	// filePath file path of resource
	filePath string
}

// ScalarSetter stores name, value and count of the scalar setter
type ScalarSetter struct {
	// Name is the name of the setter
	Name string

	// Value is the value of the field set by the setter
	Value string

	// Count is the number of fields set by the setter
	Count int
}

// ArraySetter stores name, values and count of the array setter
type ArraySetter struct {
	// Name is the name of the setter
	Name string

	// Values are the values of the field set by the setter
	Values []string

	// Count is the number of fields set by the setter
	Count int
}

// Result represents results of setter discovery
type Result struct {
	Name  string
	Value string
	Type  string
	Count int
}

func (r Result) String() string {
	return fmt.Sprintf("Name: %s, Value: %s, Type %s, Count %d", r.Name, r.Value, r.Type, r.Count)
}

// ErrSetterDiscovery represents a recoverable error that occurred during setter discovery
type ErrSetterDiscovery struct {
	message string
}

func (e *ErrSetterDiscovery) Error() string {
	return e.message
}

const (
	ArraySetterType  string = "list"
	ScalarSetterType string = "string"
)

// FindKptfile discovers Kptfile from slice of nodes
func FindKptfile(nodes []*yaml.RNode) (*kptv1.KptFile, error) {
	for _, node := range nodes {
		if node.GetAnnotations()[kioutil.PathAnnotation] == kptv1.KptFileName {
			kf, err := kptv1.ReadFile(node)
			return kf, errors.WrapPrefixf(err, "unable to read Kptfile")
		}
	}
	return nil, &ErrSetterDiscovery{"unable to find Kptfile, please include --include-meta-resources flag if a Kptfile is present"}
}

// FindSettersFromKptfile discovers setters from kptfile if exists
func FindSettersFromKptfile(nodes []*yaml.RNode) (map[string]string, error) {
	kf, err := FindKptfile(nodes)
	if err != nil {
		return nil, err
	}
	if kf.Pipeline == nil {
		return nil, &ErrSetterDiscovery{"unable to find Pipeline declaration in Kptfile"}
	}
	for _, fn := range kf.Pipeline.Mutators {
		if !strings.Contains(fn.Image, "apply-setters") {
			continue
		}
		if fn.ConfigMap != nil {
			return fn.ConfigMap, nil
		} else if fn.ConfigPath != "" {
			settersConfig, err := findSetterNode(nodes, fn.ConfigPath)
			if err != nil {
				return nil, err
			}
			return settersConfig.GetDataMap(), nil
		} else {
			return nil, &ErrSetterDiscovery{"unable to find ConfigMap or ConfigPath fnConfig for apply-setters"}
		}

	}
	return nil, &ErrSetterDiscovery{"unable to find apply-setters fn in Kptfile Pipeline.Mutators"}
}

//findSetterNode finds setter node from nodes
func findSetterNode(nodes []*yaml.RNode, path string) (*yaml.RNode, error) {
	for _, node := range nodes {
		np := node.GetAnnotations()[kioutil.PathAnnotation]
		if np == path {
			return node, nil
		}
	}
	return nil, fmt.Errorf(`file %s doesn't exist, please ensure the file specified in "configPath" exists and retry`, path)
}

// getArraySetterValues attempts to parse an array setter value
// wrapped as string to a slice of strings
func getArraySetterValues(sv string) ([]string, error) {
	rn, err := yaml.Parse(sv)
	if err != nil {
		return nil, err
	}
	elems, err := rn.Elements()
	if err != nil {
		return nil, err
	}
	setterVals := make([]string, len(elems))
	for i, elem := range elems {
		setterVal, err := elem.String()
		if err != nil {
			return nil, err
		}
		setterVals[i] = strings.ReplaceAll(setterVal, "\n", "")
	}
	return setterVals, nil
}

func New() ListSetters {
	ls := ListSetters{}
	ls.ArraySetters = make(map[string]*ArraySetter)
	ls.ScalarSetters = make(map[string]*ScalarSetter)
	return ls
}

//addKptfileSetters parses setters in fn config to ArraySetters or ScalarSetters
func (ls *ListSetters) addKptfileSetters(s map[string]string) {
	for setterName, setterValue := range s {
		v, err := getArraySetterValues(setterValue)
		if err == nil {
			ls.ArraySetters[setterName] = &ArraySetter{Name: setterName, Values: v, Count: 0}
		} else {
			ls.ScalarSetters[setterName] = &ScalarSetter{Name: setterName, Value: setterValue, Count: 0}
		}
	}
}

// GetResults returns sorted slice of all listsetter results
func (ls *ListSetters) GetResults() []*Result {
	var out []*Result
	for _, v := range ls.ArraySetters {
		out = append(out, &Result{Name: v.Name, Value: fmt.Sprint(v.Values), Count: v.Count, Type: ArraySetterType})
	}
	for _, v := range ls.ScalarSetters {
		out = append(out, &Result{Name: v.Name, Value: v.Value, Count: v.Count, Type: ScalarSetterType})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Filter implements list as a yaml.Filter
func (ls *ListSetters) Filter(nodes []*yaml.RNode) ([]*yaml.RNode, error) {
	// attempt to discover setters from Kptfile
	kfSetters, err := FindSettersFromKptfile(nodes)
	if err != nil {
		var discoveryErr *ErrSetterDiscovery
		if ok := goerrors.As(err, &discoveryErr); ok {
			ls.Warnings = append(ls.Warnings, discoveryErr)
		} else {
			return nodes, err
		}
	}
	if kfSetters != nil {
		ls.addKptfileSetters(kfSetters)
	}

	// discover setters from config
	for i := range nodes {
		filePath, _, err := kioutil.GetFileAnnotations(nodes[i])
		if err != nil {
			return nodes, err
		}
		ls.filePath = filePath
		err = accept(ls, nodes[i])
		if err != nil {
			return nil, errors.Wrap(err)
		}
	}
	return nodes, nil
}

/*
visitMapping takes input mapping node, and performs following steps
checks if the key node of the input mapping node has line comment with SetterCommentIdentifier
checks if the value node is of sequence node type
if yes to both, adds to list of ArraySetters or updates count of corresponding ArraySetter
*/
func (ls *ListSetters) visitMapping(object *yaml.RNode, path string) error {
	return object.VisitFields(func(node *yaml.MapNode) error {
		if node == nil || node.Key.IsNil() || node.Value.IsNil() {
			// don't do IsNilOrEmpty check as empty sequences are allowed
			return nil
		}

		// return if it is not a sequence node
		if node.Value.YNode().Kind != yaml.SequenceNode {
			return nil
		}

		elements, err := node.Value.Elements()
		if err != nil {
			return errors.Wrap(err)
		}

		// extracts the values in sequence node to an array
		var nodeValues []string
		for _, values := range elements {
			nodeValues = append(nodeValues, values.YNode().Value)
		}
		sort.Strings(nodeValues)

		linecomment := node.Key.YNode().LineComment
		if node.Value.YNode().Style == yaml.FlowStyle {
			linecomment = node.Value.YNode().LineComment
		}

		// perform a direct set of the field if it matches
		setterPattern := extractSetterPattern(linecomment)
		if setterPattern == "" {
			// the node is not tagged with setter pattern
			return nil
		}

		// add setter to discovered array setters or update count of existing setter
		setterName := clean(setterPattern)
		_, ok := ls.ArraySetters[setterName]
		if ok {
			ls.ArraySetters[setterName].Count += 1
		} else {
			ls.ArraySetters[setterName] = &ArraySetter{Name: setterName, Values: nodeValues, Count: 1}
		}
		return nil
	})
}

/*
visitScalar accepts the input scalar node and performs following steps,
checks if the line comment of input scalar node has prefix SetterCommentIdentifier
adds to list of ScalarSetters or updates count of corresponding ScalarSetter
*/
func (ls *ListSetters) visitScalar(object *yaml.RNode, path string) error {
	if object.IsNil() {
		return nil
	}

	if object.YNode().Kind != yaml.ScalarNode {
		// return if it is not a scalar node
		return nil
	}

	linecomment := object.YNode().LineComment

	// perform a direct set of the field if it matches
	setterPattern := extractSetterPattern(linecomment)
	if setterPattern == "" {
		// the node is not tagged with setter pattern
		return nil
	}
	currentSetterValues := currentSetterValues(setterPattern, object.YNode().Value)

	// add setters to discovered scalar setters or update count of existing setter
	for setterName, setterValue := range currentSetterValues {
		_, ok := ls.ScalarSetters[setterName]
		if ok {
			ls.ScalarSetters[setterName].Count++
		} else {
			ls.ScalarSetters[setterName] = &ScalarSetter{Name: setterName, Value: setterValue, Count: 1}
		}

	}
	return nil
}

// extractSetterPattern extracts the setter pattern from the line comment of the
// yaml RNode. If the the line comment doesn't contain SetterCommentIdentifier
// prefix, then it returns empty string
func extractSetterPattern(lineComment string) string {
	if !strings.HasPrefix(lineComment, SetterCommentIdentifier) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(lineComment, SetterCommentIdentifier))
}

// currentSetterValues takes pattern and value and returns setter names to values
// derived using pattern matching
// e.g. pattern = my-app-layer.${stage}.${domain}.${tld}, value = my-app-layer.dev.example.com
// returns {"stage":"dev", "domain":"example", "tld":"com"}
func currentSetterValues(pattern, value string) map[string]string {
	res := make(map[string]string)
	// get all setter names enclosed in ${}
	// e.g. value: my-app-layer.dev.example.com
	// pattern: my-app-layer.${stage}.${domain}.${tld}
	// urs: [${stage}, ${domain}, ${tld}]
	urs := unresolvedSetters(pattern)
	// and escape pattern
	pattern = regexp.QuoteMeta(pattern)
	// escaped pattern: my-app-layer\.\$\{stage\}\.\$\{domain\}\.\$\{tld\}

	for _, setterName := range urs {
		// escape setter name
		// we need to escape the setterName as well to replace it in the escaped pattern string later
		setterName = regexp.QuoteMeta(setterName)
		pattern = strings.ReplaceAll(
			pattern,
			setterName,
			`(?P<x>.*)`) // x is just a place holder, it could be any alphanumeric string
	}
	// pattern: my-app-layer\.(?P<x>.*)\.(?P<x>.*)\.(?P<x>.*)
	r, err := regexp.Compile(pattern)
	if err != nil {
		// just return empty map if values can't be derived from pattern
		return res
	}
	setterValues := r.FindStringSubmatch(value)
	if len(setterValues) == 0 {
		return res
	}
	// setterValues: [ "my-app-layer.dev.example.com", "dev", "example", "com"]
	setterValues = setterValues[1:]
	// setterValues: [ "dev", "example", "com"]
	if len(urs) != len(setterValues) {
		// just return empty map if values can't be derived
		return res
	}
	for i := range setterValues {
		if setterValues[i] == "" {
			// if any of the value is unresolved return empty map
			// and expect users to provide all values
			return make(map[string]string)
		}
		res[clean(urs[i])] = setterValues[i]
	}
	return res
}

// unresolvedSetters returns the list of values enclosed in ${} present within given
// pattern e.g. pattern = foo-${image}:${tag}-bar return ["${image}", "${tag}"]
func unresolvedSetters(pattern string) []string {
	re := regexp.MustCompile(`\$\{([^}]*)\}`)
	return re.FindAllString(pattern, -1)
}

// clean extracts value enclosed in ${}
func clean(input string) string {
	input = strings.TrimSpace(input)
	return strings.TrimSuffix(strings.TrimPrefix(input, "${"), "}")
}
