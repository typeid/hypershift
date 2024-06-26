/*


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
// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "github.com/openshift/hypershift/api/hypershift/v1alpha1"
)

// SecretEncryptionSpecApplyConfiguration represents an declarative configuration of the SecretEncryptionSpec type for use
// with apply.
type SecretEncryptionSpecApplyConfiguration struct {
	Type   *v1alpha1.SecretEncryptionType `json:"type,omitempty"`
	KMS    *KMSSpecApplyConfiguration     `json:"kms,omitempty"`
	AESCBC *AESCBCSpecApplyConfiguration  `json:"aescbc,omitempty"`
}

// SecretEncryptionSpecApplyConfiguration constructs an declarative configuration of the SecretEncryptionSpec type for use with
// apply.
func SecretEncryptionSpec() *SecretEncryptionSpecApplyConfiguration {
	return &SecretEncryptionSpecApplyConfiguration{}
}

// WithType sets the Type field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Type field is set to the value of the last call.
func (b *SecretEncryptionSpecApplyConfiguration) WithType(value v1alpha1.SecretEncryptionType) *SecretEncryptionSpecApplyConfiguration {
	b.Type = &value
	return b
}

// WithKMS sets the KMS field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the KMS field is set to the value of the last call.
func (b *SecretEncryptionSpecApplyConfiguration) WithKMS(value *KMSSpecApplyConfiguration) *SecretEncryptionSpecApplyConfiguration {
	b.KMS = value
	return b
}

// WithAESCBC sets the AESCBC field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the AESCBC field is set to the value of the last call.
func (b *SecretEncryptionSpecApplyConfiguration) WithAESCBC(value *AESCBCSpecApplyConfiguration) *SecretEncryptionSpecApplyConfiguration {
	b.AESCBC = value
	return b
}
