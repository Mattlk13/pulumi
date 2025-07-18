// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
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
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
)

// Goal is a desired state for a resource object.  Normally it represents a subset of the resource's state expressed by
// a program, however if Output is true, it represents a more complete, post-deployment view of the state.
type Goal struct {
	Type                    tokens.Type           // the type of resource.
	Name                    string                // the name for the resource's URN.
	Custom                  bool                  // true if this resource is custom, managed by a plugin.
	Properties              PropertyMap           // the resource's property state.
	Parent                  URN                   // an optional parent URN for this resource.
	Protect                 *bool                 // true to protect this resource from deletion.
	Dependencies            []URN                 // dependencies of this resource object.
	Provider                string                // the provider to use for this resource.
	InitErrors              []string              // errors encountered as we attempted to initialize the resource.
	PropertyDependencies    map[PropertyKey][]URN // the set of dependencies that affect each property.
	DeleteBeforeReplace     *bool                 // true if this resource should be deleted prior to replacement.
	IgnoreChanges           []string              // a list of property paths to ignore when diffing.
	AdditionalSecretOutputs []PropertyKey         // outputs that should always be treated as secrets.
	Aliases                 []Alias               // additional structured Aliases that should be assigned.
	ID                      ID                    // the expected ID of the resource, if any.
	CustomTimeouts          CustomTimeouts        // an optional config object for resource options
	ReplaceOnChanges        []string              // a list of property paths that if changed should force a replacement.
	// if set to True, the providers Delete method will not be called for this resource.
	RetainOnDelete *bool
	// if set, the providers Delete method will not be called for this resource
	// if specified resource is being deleted as well.
	DeletedWith    URN
	SourcePosition string                // If set, the source location of the resource registration
	ResourceHooks  map[HookType][]string // The resource hooks attached to the resource, by type.
}

// NewGoal allocates a new resource goal state.
func NewGoal(t tokens.Type, name string, custom bool, props PropertyMap,
	parent URN, protect *bool, dependencies []URN, provider string, initErrors []string,
	propertyDependencies map[PropertyKey][]URN, deleteBeforeReplace *bool, ignoreChanges []string,
	additionalSecretOutputs []PropertyKey, aliases []Alias, id ID, customTimeouts *CustomTimeouts,
	replaceOnChanges []string, retainOnDelete *bool, deletedWith URN, sourcePosition string,
	resourceHooks map[HookType][]string,
) *Goal {
	g := &Goal{
		Type:                    t,
		Name:                    name,
		Custom:                  custom,
		Properties:              props,
		Parent:                  parent,
		Protect:                 protect,
		Dependencies:            dependencies,
		Provider:                provider,
		InitErrors:              initErrors,
		PropertyDependencies:    propertyDependencies,
		DeleteBeforeReplace:     deleteBeforeReplace,
		IgnoreChanges:           ignoreChanges,
		AdditionalSecretOutputs: additionalSecretOutputs,
		Aliases:                 aliases,
		ID:                      id,
		ReplaceOnChanges:        replaceOnChanges,
		RetainOnDelete:          retainOnDelete,
		DeletedWith:             deletedWith,
		SourcePosition:          sourcePosition,
		ResourceHooks:           resourceHooks,
	}

	if customTimeouts != nil {
		g.CustomTimeouts = *customTimeouts
	}

	return g
}
