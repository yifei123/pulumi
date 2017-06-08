// Licensed to Pulumi Corporation ("Pulumi") under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// Pulumi licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resource

import (
	"crypto/rand"
	"encoding/hex"
	"reflect"

	"github.com/golang/glog"

	"github.com/pulumi/lumi/pkg/compiler/symbols"
	"github.com/pulumi/lumi/pkg/compiler/types"
	"github.com/pulumi/lumi/pkg/compiler/types/predef"
	"github.com/pulumi/lumi/pkg/eval/heapstate"
	"github.com/pulumi/lumi/pkg/eval/rt"
	"github.com/pulumi/lumi/pkg/tokens"
	"github.com/pulumi/lumi/pkg/util/contract"
)

// ID is a unique resource identifier; it is managed by the provider and is mostly opaque to Lumi.
type ID string

func MaybeID(s *string) *ID {
	var ret *ID
	if s != nil {
		id := ID(*s)
		ret = &id
	}
	return ret
}

func (id ID) String() string { return string(id) }
func (id *ID) StringPtr() *string {
	if id == nil {
		return nil
	}
	ids := (*id).String()
	return &ids
}
func IDStrings(ids []ID) []string {
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = id.String()
	}
	return ss
}

// Resource is an instance of a resource with an ID, type, and bag of state.
type Resource interface {
	ID() ID                      // the resource's unique ID assigned by the provider (or blank if uncreated).
	URN() URN                    // the resource's object urn, a human-friendly, unique name for the resource.
	Type() tokens.Type           // the resource's type.
	Inputs() PropertyMap         // the resource's input properties (as specified by the program).
	Outputs() PropertyMap        // the resource's output properties (as specified by the resource provider).
	HasID() bool                 // returns true if the resource has been assigned an ID.
	SetID(id ID)                 // assignes an ID to this resource, for those under creation.
	HasURN() bool                // returns true if the resource has been assigned URN.
	SetURN(m URN)                // assignes a URN to this resource, for those under creation.
	SetOutputsFrom(src Resource) // copy all output properties from one resource to another.
	ShallowClone() Resource      // make a shallow clone of the resource.
}

// State is returned when an error has occurred during a resource provider operation.  It indicates whether the
// operation could be rolled back cleanly (OK).  If not, it means the resource was left in an indeterminate state.
type State int

const (
	StateOK State = iota
	StateUnknown
)

// IsResourceVertex returns true if the heap graph vertex has an object whose type is the standard resource class.
func IsResourceVertex(v *heapstate.ObjectVertex) bool {
	return predef.IsResourceType(v.Obj().Type())
}

type resource struct {
	id      ID          // the resource's unique ID, assigned by the resource provider (or blank if uncreated).
	urn     URN         // the resource's object urn, a human-friendly, unique name for the resource.
	t       tokens.Type // the resource's type.
	inputs  PropertyMap // the resource's input properties (as specified by the program).
	outputs PropertyMap // the resource's output properties (as specified by the resource provider).
}

func (r *resource) ID() ID               { return r.id }
func (r *resource) URN() URN             { return r.urn }
func (r *resource) Type() tokens.Type    { return r.t }
func (r *resource) Inputs() PropertyMap  { return r.inputs }
func (r *resource) Outputs() PropertyMap { return r.outputs }

func (r *resource) HasID() bool { return (string(r.id) != "") }
func (r *resource) SetID(id ID) {
	contract.Requiref(!r.HasID(), "id", "empty")
	glog.V(9).Infof("Assigning ID=%v to resource w/ URN=%v", id, r.urn)
	r.id = id
}

func (r *resource) HasURN() bool { return (string(r.urn) != "") }
func (r *resource) SetURN(m URN) {
	contract.Requiref(!r.HasURN(), "urn", "empty")
	r.urn = m
}

// SetOutputsFrom copies all output properties from a src resource to the instance.
func (r *resource) SetOutputsFrom(src Resource) {
	src.Outputs().ShallowCloneInto(r.Outputs())
}

// ShallowClone clones a resource object so that any modifications to it are not reflected in the original.  Note that
// the property map is only shallowly cloned so any mutations deep within it may get reflected in the original.
func (r *resource) ShallowClone() Resource {
	return &resource{
		id:      r.id,
		urn:     r.urn,
		t:       r.t,
		inputs:  r.inputs.ShallowClone(),
		outputs: r.outputs.ShallowClone(),
	}
}

// NewResource creates a new resource from the information provided.
func NewResource(id ID, urn URN, t tokens.Type, inputs PropertyMap, outputs PropertyMap) Resource {
	if inputs == nil {
		inputs = make(PropertyMap)
	}
	if outputs == nil {
		outputs = make(PropertyMap)
	}
	return &resource{
		id:      id,
		urn:     urn,
		t:       t,
		inputs:  inputs,
		outputs: outputs,
	}
}

// NewObjectResource creates a new resource object out of the runtime object provided.  The context is used to resolve
// dependencies between resources and must contain all references that could be encountered.
func NewObjectResource(ctx *Context, obj *rt.Object) Resource {
	// Ensure this is actually a resource type first.
	t := obj.Type()
	contract.Assert(predef.IsResourceType(t))

	// Do a deep copy of the resource properties.  This ensures property serializability.
	props := cloneResource(ctx, obj)

	// Finally allocate and return the resource object; note that ID and URN are blank until the provider assigns them.
	return NewResource("", "", t.TypeToken(), props, nil)
}

// cloneResource creates a property map out of a resource's runtime object.
func cloneResource(ctx *Context, resobj *rt.Object) PropertyMap {
	return cloneObject(ctx, resobj, resobj)
}

// cloneObject creates a property map out of a runtime object.  The result is fully serializable in the sense that it
// can be stored in a JSON or YAML file, serialized over an RPC interface, etc.  In particular, any references to other
// resources are replaced with their urn equivalents, which the runtime understands.
func cloneObject(ctx *Context, resobj *rt.Object, obj *rt.Object) PropertyMap {
	contract.Assert(obj != nil)
	props := obj.PropertyValues()
	return cloneObjectProperties(ctx, resobj, props)
}

// cloneObjectProperty creates a single property value out of a runtime object.  It returns false if the property could
// not be stored in a property (e.g., it is a function or other unrecognized or unserializable runtime object).
func cloneObjectProperty(ctx *Context, resobj *rt.Object, obj *rt.Object) (PropertyValue, bool) {
	t := obj.Type()

	// Serialize resource references as URNs.
	if predef.IsResourceType(t) {
		// For resources, simply look up the urn from the resource map.
		urn, hasm := ctx.ObjURN[obj]
		contract.Assertf(hasm, "Missing object reference for %v; possible out of order dependency walk", obj)
		return NewResourceProperty(urn), true
	}

	// Serialize simple primitive types with their primitive equivalents.
	switch t {
	case types.Null:
		return NewNullProperty(), true
	case types.Bool:
		return NewBoolProperty(obj.BoolValue()), true
	case types.Number:
		return NewNumberProperty(obj.NumberValue()), true
	case types.String:
		return NewStringProperty(obj.StringValue()), true
	case types.Object, types.Dynamic:
		result := cloneObject(ctx, resobj, obj) // an object literal, clone it
		return NewObjectProperty(result), true
	}

	// Serialize arrays, maps, and object instances in the obvious way.
	switch t.(type) {
	case *symbols.ArrayType:
		// Make a new array, clone each element, and return the result.
		var result []PropertyValue
		for _, e := range *obj.ArrayValue() {
			if v, ok := cloneObjectProperty(ctx, obj, e.Obj()); ok {
				result = append(result, v)
			}
		}
		return NewArrayProperty(result), true
	case *symbols.MapType:
		// Make a new map, clone each property value, and return the result.
		props := obj.PropertyValues()
		result := cloneObjectProperties(ctx, resobj, props)
		return NewObjectProperty(result), true
	case *symbols.Class:
		// Make a new object that contains a deep clone of the source.
		result := cloneObject(ctx, resobj, obj)
		return NewObjectProperty(result), true
	}

	// If a computed value, we can propagate an unknown value, but only for certain cases.
	if t.Computed() {
		// If this is an output property, then this property will turn into an output.  Otherwise, it will be marked
		// completed.  An output property is permitted in more places by virtue of the fact that it is expected not to
		// exist during resource create operations, whereas all computed properties should have been resolved by then.
		comp := obj.ComputedValue()
		outprop := (!comp.Expr && len(comp.Sources) == 1 && comp.Sources[0] == resobj)
		var makeProperty func(PropertyValue) PropertyValue
		if outprop {
			// For output properties, we need not track any URNs.
			makeProperty = func(v PropertyValue) PropertyValue {
				return MakeOutput(v)
			}
		} else {
			// For all other properties, we need to look up and store the URNs for all resource dependencies.
			var urns []URN
			for _, src := range comp.Sources {
				// For all inter-resource references, materialize a URN.  Skip this for intra-resource references!
				if src != resobj {
					urn, hasm := ctx.ObjURN[src]
					contract.Assertf(hasm,
						"Missing computed reference from %v to %v; possible out of order dependency walk", resobj, src)
					urns = append(urns, urn)
				}
			}
			makeProperty = func(v PropertyValue) PropertyValue {
				return MakeComputed(v, urns)
			}
		}

		future := t.(*symbols.ComputedType).Element
		switch future {
		case types.Null:
			return makeProperty(NewNullProperty()), true
		case types.Bool:
			return makeProperty(NewBoolProperty(false)), true
		case types.Number:
			return makeProperty(NewNumberProperty(0)), true
		case types.String:
			return makeProperty(NewStringProperty("")), true
		case types.Object, types.Dynamic:
			return makeProperty(NewObjectProperty(make(PropertyMap))), true
		}
		switch future.(type) {
		case *symbols.ArrayType:
			return makeProperty(NewArrayProperty(nil)), true
		case *symbols.Class:
			return makeProperty(NewObjectProperty(make(PropertyMap))), true
		}
	}

	// We can safely skip serializing functions, however, anything else is unexpected at this point.
	_, isfunc := t.(*symbols.FunctionType)
	contract.Assertf(isfunc, "Unrecognized resource property object type '%v' (%v)", t, reflect.TypeOf(t))
	return PropertyValue{}, false
}

// cloneObjectProperties copies a resource's properties.
func cloneObjectProperties(ctx *Context, resobj *rt.Object, props *rt.PropertyMap) PropertyMap {
	// Walk the object's properties and serialize them in a stable order.
	result := make(PropertyMap)
	for _, k := range props.Stable() {
		if v, ok := cloneObjectProperty(ctx, resobj, props.Get(k)); ok {
			result[PropertyKey(k)] = v
		}
	}
	return result
}

// NewUniqueHex generates a new "random" hex string for use by resource providers.  It has the given optional prefix and
// the total length is capped to the maxlen.  Note that capping to maxlen necessarily increases the risk of collisions.
func NewUniqueHex(prefix string, randlen, maxlen int) string {
	bs := make([]byte, randlen)
	n, err := rand.Read(bs)
	contract.Assert(err == nil)
	contract.Assert(n == len(bs))

	str := prefix + hex.EncodeToString(bs)
	if len(str) > maxlen {
		str = str[:maxlen]
	}
	return str
}

// NewUniqueHexID generates a new "random" hex ID for use by resource providers.  It has the given optional prefix and
// the total length is capped to the maxlen.  Note that capping to maxlen necessarily increases the risk of collisions.
func NewUniqueHexID(prefix string, randlen, maxlen int) ID {
	return ID(NewUniqueHex(prefix, randlen, maxlen))
}
