/*
Copyright 2024 The Kubernetes Authors.

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

package crusoecloud

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"go.uber.org/multierr"
)

const (
	operationBackoffIntervalDefault    = 1
	operationBackoffJitterRangeDefault = 1000
)

var (
	errorOperationDidNotSucceed = errors.New("operation did not succeed")
)

type waitBackoff struct {
	backoffIntervalSecs  float32
	backoffJitterRangeMs int
}

func newWaitBackoff(intervalSecs float32, jitterRangeMs int) *waitBackoff {
	return &waitBackoff{
		backoffIntervalSecs:  intervalSecs,
		backoffJitterRangeMs: jitterRangeMs,
	}
}

func newDefaultWaitBackoff() *waitBackoff {
	return newWaitBackoff(operationBackoffIntervalDefault, operationBackoffJitterRangeDefault)
}

type pollOpFunc func(ctx context.Context, operationId string) (*crusoeapi.Operation, error)

func (w *waitBackoff) WaitForOperationListComplete(ctx context.Context, ops []*crusoeapi.Operation, pollOp pollOpFunc) (
	[]*crusoeapi.Operation, error,
) {
	if len(ops) == 0 { // ignore empty list
		return nil, nil
	}

	var wg sync.WaitGroup
	wg.Add(len(ops))
	results := make([]*crusoeapi.Operation, len(ops))
	errs := make([]error, len(ops))
	for i, op := range ops {
		go func(i int, op *crusoeapi.Operation) {
			defer wg.Done()
			result, opErr := w.WaitForOperationComplete(ctx, op, pollOp)
			results[i] = result
			errs[i] = opErr
		}(i, op)
	}
	wg.Wait()

	var multiErr error
	for i, err := range errs {
		if err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("failed to check operation %d: %w", i, err))
		}
	}
	for i, op := range results {
		if op.State == string(opFailed) {
			multiErr = multierr.Append(multiErr, fmt.Errorf("failed to complete operation %d (result %v): %w", i, op.Result, errorOperationDidNotSucceed))
		}
	}

	// If any of the operations failed, return the multi-error.
	if multiErr != nil {
		return nil, multiErr
	}

	return results, nil
}

// AwaitOperationComplete waits for an operation to complete, and returns the operation
// result. If the operation fails, an error is returned. Generalized for any operation types with pollOp.
func (w *waitBackoff) WaitForOperationComplete(ctx context.Context, op *crusoeapi.Operation, pollOp pollOpFunc) (
	*crusoeapi.Operation, error,
) {
	// ignore no-op case
	if op == nil {
		return nil, nil
	}

	for op.State == string(opInProgress) {
		updatedOp, err := pollOp(ctx, op.OperationId)
		if err != nil {
			return nil, fmt.Errorf("error getting operation with id %s: %w", op.OperationId, err)
		}
		op = updatedOp

		time.Sleep(time.Duration(w.backoffIntervalSecs)*time.Second + jitterMillisecond(w.backoffJitterRangeMs))
	}

	return op, nil
}

func jitterMillisecond(maxVal int) time.Duration {
	if maxVal == 0 {
		return time.Duration(0)
	}
	bigRand, err := rand.Int(rand.Reader, big.NewInt(int64(maxVal)))
	if err != nil {
		return time.Duration(0)
	}

	return time.Duration(bigRand.Int64()) * time.Millisecond
}
