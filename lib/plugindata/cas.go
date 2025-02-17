// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugindata

import (
	"context"
	"time"

	"github.com/gravitational/teleport-plugins/lib/backoff"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
)

const (
	// backoffBase is an initial (minimum) backoff value.
	backoffBase = time.Millisecond
	// backoffMax is a backoff threshold
	backoffMax = time.Second
)

// Client represents an interface to Teleport API client
type Client interface {
	GetPluginData(context.Context, types.PluginDataFilter) ([]types.PluginData, error)
	UpdatePluginData(context.Context, types.PluginDataUpdateParams) error
}

// CompareAndSwap represents modifier struct
type CompareAndSwap[T any] struct {
	client      Client
	name        string
	kind        string
	backoffBase time.Duration
	backoffMax  time.Duration
	encode      func(T) (map[string]string, error)
	decode      func(map[string]string) (T, error)
}

// NewCAS returns modifier struct
func NewCAS[T any](
	client Client, name,
	kind string,
	encode func(T) (map[string]string, error),
	decode func(map[string]string) (T, error),
) *CompareAndSwap[T] {
	return &CompareAndSwap[T]{
		client,
		name,
		kind,
		backoffBase,
		backoffMax,
		encode,
		decode,
	}
}

// Create tries to perform compare-and-swap update of a plugin data assuming that it does not exist
//
// fn callback function receives current plugin data value and returns modified value and
// error.
//
// Please note that fn might be called several times due to CAS backoff, hence, you must be careful
// with things like I/O ops and channels.
func (c *CompareAndSwap[T]) Create(
	ctx context.Context,
	resource string,
	newData T,
) (T, error) {
	emptyData := *new(T)

	existingData, err := c.getPluginData(ctx, resource)
	if err != nil && !trace.IsNotFound(err) {
		return emptyData, trace.Wrap(err)
	}

	if existingData != nil {
		return emptyData, trace.AlreadyExists("plugin data already exists")
	}

	err = c.updatePluginData(ctx, resource, newData, emptyData)
	if err == nil {
		return newData, nil
	}

	return emptyData, trace.Wrap(err)
}

// Update tries to perform compare-and-swap update of a plugin data assuming that it exist
//
// modifyT will receive existing plugin data and should return a modified version of the data.

// If existing plugin data does not match expected data, then a trace.CompareFailed error should
// be returned to backoff and try again.

// To abort the update, modifyT should return an error other, than trace.CompareFailed, which
// will be propagated back to the caller of `Update`.
func (c *CompareAndSwap[T]) Update(
	ctx context.Context,
	resource string,
	modifyT func(T) (T, error),
) (T, error) {
	emptyData := *new(T)
	var failedAttempts []error

	backoff := backoff.NewDecorr(c.backoffBase, c.backoffMax, clockwork.NewRealClock())
	for {
		// Get existing data
		oldData, err := c.getPluginData(ctx, resource)
		if err != nil {
			return emptyData, trace.Wrap(err)
		}

		cbData := *oldData
		expectData := *oldData

		// Modify data
		newData, err := modifyT(cbData)
		if trace.IsCompareFailed(err) {
			failedAttempts = append(failedAttempts, trace.Wrap(err))
			backoffErr := backoff.Do(ctx)
			if backoffErr != nil {
				failedAttempts = append(failedAttempts, trace.Wrap(backoffErr))
				return emptyData, trace.NewAggregate(failedAttempts...)
			}

			continue
		} else if err != nil {
			return emptyData, trace.Wrap(err)
		}

		// Submit modifications
		err = c.updatePluginData(ctx, resource, newData, expectData)
		if err == nil {
			return newData, nil
		}
		if !trace.IsCompareFailed(err) {
			return emptyData, trace.Wrap(err)
		}
		// A conflict happened, we register the failed attempt and wait before retrying
		failedAttempts = append(failedAttempts, trace.Wrap(err))
		backoffErr := backoff.Do(ctx)
		if backoffErr != nil {
			failedAttempts = append(failedAttempts, trace.Wrap(backoffErr))
			return emptyData, trace.NewAggregate(failedAttempts...)
		}
	}
}

// NOTE: Implement Upsert method when it will be required

// getPluginData loads a plugin data for a given resource. It returns nil if it's not found.
func (c *CompareAndSwap[T]) getPluginData(ctx context.Context, resource string) (*T, error) {
	dataMaps, err := c.client.GetPluginData(ctx, types.PluginDataFilter{
		Kind:     c.kind,
		Resource: resource,
		Plugin:   c.name,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if len(dataMaps) == 0 {
		return nil, trace.NotFound("plugin data not found")
	}
	entry := dataMaps[0].Entries()[c.name]
	if entry == nil || entry.Data == nil {
		return nil, trace.NotFound("plugin data entry not found")
	}
	d, err := c.decode(entry.Data)
	return &d, err
}

// updatePluginData updates an existing plugin data or sets a new one if it didn't exist.
func (c *CompareAndSwap[T]) updatePluginData(ctx context.Context, resource string, data T, expectData T) error {
	set, err := c.encode(data)
	if err != nil {
		return err
	}
	expect, err := c.encode(expectData)
	if err != nil {
		return err
	}
	return c.client.UpdatePluginData(ctx, types.PluginDataUpdateParams{
		Kind:     c.kind,
		Resource: resource,
		Plugin:   c.name,
		Set:      set,
		Expect:   expect,
	})
}
