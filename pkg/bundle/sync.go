/*
Copyright 2021 The cert-manager Authors.

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

package bundle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trustapi "github.com/cert-manager/trust-manager/pkg/apis/trust/v1alpha1"
	"github.com/cert-manager/trust-manager/pkg/util"
)

type notFoundError struct{ error }

// buildSourceBundle retrieves and appends all source bundle data for this
// Bundle object.
// Each source data has its space trimmed, and is appended by a new line character.
func (b *bundle) buildSourceBundle(ctx context.Context, bundle *trustapi.Bundle) (string, error) {
	var data []string

	for _, source := range bundle.Spec.Sources {
		var (
			sourceData string
			err        error
		)

		switch {
		case source.ConfigMap != nil:
			sourceData, err = b.configMapBundle(ctx, source.ConfigMap)

		case source.Secret != nil:
			sourceData, err = b.secretBundle(ctx, source.Secret)

		case source.InLine != nil:
			sourceData = *source.InLine
		}

		if err != nil {
			return "", fmt.Errorf("failed to retrieve bundle from source: %w", err)
		}

		sanitizedBundle, err := util.ValidateAndSanitizePEMBundle([]byte(sourceData))
		if err != nil {
			return "", fmt.Errorf("invalid PEM data in source: %w", err)
		}

		data = append(data, string(sanitizedBundle))
	}

	// return early to prevent returning just newline
	if len(data) == 0 {
		return "", nil
	}

	return strings.Join(data, "\n") + "\n", nil
}

// configMapBundle returns the data in the target ConfigMap within the trust
// Namespace.
func (b *bundle) configMapBundle(ctx context.Context, ref *trustapi.SourceObjectKeySelector) (string, error) {
	var configMap corev1.ConfigMap
	err := b.client.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ref.Name}, &configMap)
	if apierrors.IsNotFound(err) {
		return "", notFoundError{err}
	}

	if err != nil {
		return "", fmt.Errorf("failed to get ConfigMap %s/%s: %w", b.Namespace, ref.Name, err)
	}

	data, ok := configMap.Data[ref.Key]
	if !ok {
		return "", notFoundError{fmt.Errorf("no data found in ConfigMap %s/%s at key %q", b.Namespace, ref.Name, ref.Key)}
	}

	return data, nil
}

// secretBundle returns the data in the target Secret within the trust
// Namespace.
func (b *bundle) secretBundle(ctx context.Context, ref *trustapi.SourceObjectKeySelector) (string, error) {
	var secret corev1.Secret
	err := b.client.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ref.Name}, &secret)
	if apierrors.IsNotFound(err) {
		return "", notFoundError{err}
	}
	if err != nil {
		return "", fmt.Errorf("failed to get Secret %s/%s: %w", b.Namespace, ref.Name, err)
	}

	data, ok := secret.Data[ref.Key]
	if !ok {
		return "", notFoundError{fmt.Errorf("no data found in Secret %s/%s at key %q", b.Namespace, ref.Name, ref.Key)}
	}

	return string(data), nil
}

// syncTarget syncs the given data to the target ConfigMap in the given
// namespace.
// The name of the ConfigMap is the same as the Bundle.
// Ensures the ConfigMap is owned by the given Bundle, and the data is up to
// date.
// Returns true if the ConfigMap has been created or was updated.
func (b *bundle) syncTarget(ctx context.Context, log logr.Logger,
	bundle *trustapi.Bundle,
	namespaceSelector labels.Selector,
	namespace *corev1.Namespace,
	data string,
) (bool, error) {
	target := bundle.Spec.Target

	if target.ConfigMap == nil {
		return false, errors.New("target not defined")
	}

	matchNamespace := namespaceSelector.Matches(labels.Set(namespace.Labels))

	var configMap corev1.ConfigMap
	err := b.client.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: bundle.Name}, &configMap)

	// If the ConfigMap doesn't exist yet, create it.
	if apierrors.IsNotFound(err) {
		// If the namespace doesn't match selector we do nothing since we don't
		// want to create it, and it also doesn't exist.
		if !matchNamespace {
			log.V(4).Info("ignoring namespace as it doesn't match selector", "labels", namespace.Labels)
			return false, nil
		}

		configMap = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            bundle.Name,
				Namespace:       namespace.Name,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bundle, trustapi.SchemeGroupVersion.WithKind("Bundle"))},
			},
			Data: map[string]string{
				target.ConfigMap.Key: data,
			},
		}

		return true, b.client.Create(ctx, &configMap)
	}

	if err != nil {
		return false, fmt.Errorf("failed to get configmap %s/%s: %w", namespace, bundle.Name, err)
	}

	// Here, the config map exists, but the selector doesn't match the namespace.
	if !matchNamespace {
		// The ConfigMap is owned by this controller- delete it.
		if metav1.IsControlledBy(&configMap, bundle) {
			log.V(2).Info("deleting bundle from Namespace since namespaceSelector does not match")
			return true, b.client.Delete(ctx, &configMap)
		}
		// The ConfigMap isn't owned by us, so we shouldn't delete it. Return that
		// we did nothing.
		b.recorder.Eventf(&configMap, corev1.EventTypeWarning, "NotOwned", "ConfigMap is not owned by trust.cert-manager.io so ignoring")
		return false, nil
	}

	var needsUpdate bool
	// If ConfigMap is missing OwnerReference, add it back.
	if !metav1.IsControlledBy(&configMap, bundle) {
		configMap.OwnerReferences = append(configMap.OwnerReferences, *metav1.NewControllerRef(bundle, trustapi.SchemeGroupVersion.WithKind("Bundle")))
		needsUpdate = true
	}

	// Match, return do nothing
	if cmdata, ok := configMap.Data[target.ConfigMap.Key]; !ok || cmdata != data {
		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}
		configMap.Data[target.ConfigMap.Key] = data
		needsUpdate = true
	}

	// Exit early if no update is needed
	if !needsUpdate {
		return false, nil
	}

	if err := b.client.Update(ctx, &configMap); err != nil {
		return true, fmt.Errorf("failed to update configmap %s/%s with bundle: %w", namespace, bundle.Name, err)
	}

	log.V(2).Info("synced bundle to namespace")

	return true, nil
}
