/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package activator

const (
	// Name is the name of the component.
	Name = "activator"
	// RevisionHeaderName is the header key for revision name.
	RevisionHeaderName = "Knative-Serving-Revision"
	// RevisionHeaderNamespace is the header key for revision's namespace.
	RevisionHeaderNamespace = "Knative-Serving-Namespace"
)

// RevisionHeaders are the headers the activator uses to identify the
// revision. They are removed before reaching the user container.
var RevisionHeaders = []string{
	RevisionHeaderName,
	RevisionHeaderNamespace,
}
