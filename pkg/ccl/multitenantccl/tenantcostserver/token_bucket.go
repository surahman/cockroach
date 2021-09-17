// Copyright 2021 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package tenantcostserver

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

// TokenBucketRequest is part of the multitenant.TenantUsageServer and
// implements the TokenBucket API of the roachpb.Internal service. This code
// runs on the host cluster to service requests coming from tenants (through the
// kvtenant.Connector).
func (s *instance) TokenBucketRequest(
	ctx context.Context, tenantID roachpb.TenantID, in *roachpb.TokenBucketRequest,
) *roachpb.TokenBucketResponse {
	if tenantID == roachpb.SystemTenantID {
		return &roachpb.TokenBucketResponse{
			Error: errors.EncodeError(ctx, errors.New("token bucket request for system tenant")),
		}
	}
	instanceID := base.SQLInstanceID(in.InstanceID)
	if instanceID < 1 {
		return &roachpb.TokenBucketResponse{
			Error: errors.EncodeError(ctx, errors.Errorf("invalid instance ID %d", instanceID)),
		}
	}
	if in.RequestedRU < 0 {
		return &roachpb.TokenBucketResponse{
			Error: errors.EncodeError(ctx, errors.Errorf("negative requested RUs")),
		}
	}

	metrics := s.metrics.getTenantMetrics(tenantID)
	// Use a per-tenant mutex to serialize operations to the bucket. The
	// transactions will need to be serialized anyway, so this avoids more
	// expensive restarts. It also guarantees that the metric updates happen in
	// the same order with the system table changes.
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()

	result := &roachpb.TokenBucketResponse{}
	var consumption roachpb.TenantConsumption
	if err := s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		*result = roachpb.TokenBucketResponse{}

		h := makeSysTableHelper(ctx, s.executor, txn, tenantID)
		tenant, instance, err := h.readTenantAndInstanceState(instanceID)
		if err != nil {
			return err
		}

		now := s.timeSource.Now()
		tenant.update(now)

		if !instance.Present {
			if err := h.accomodateNewInstance(&tenant, &instance); err != nil {
				return err
			}
		}
		if string(instance.Lease) != string(in.InstanceLease) {
			// This must be a different incarnation of the same ID. Clear the sequence
			// number.
			instance.Seq = 0
			instance.Lease = tree.DBytes(in.InstanceLease)
		}

		// Only update consumption if we are sure this is not a duplicate request
		// that we already counted. Note that if this is a duplicate request, it
		// will still consume RUs; we rely on a higher level control loop that
		// periodically reconfigures the token bucket to correct such errors.
		if instance.Seq == 0 || instance.Seq < in.SeqNum {
			tenant.Consumption.Add(&in.ConsumptionSinceLastRequest)
			instance.Seq = in.SeqNum
		}

		// TODO(radu): update shares.
		*result = tenant.Bucket.Request(in)

		instance.LastUpdate.Time = now
		if err := h.updateTenantAndInstanceState(tenant, instance); err != nil {
			return err
		}

		if err := h.maybeCheckInvariants(ctx); err != nil {
			panic(err)
		}
		consumption = tenant.Consumption
		return nil
	}); err != nil {
		return &roachpb.TokenBucketResponse{
			Error: errors.EncodeError(ctx, err),
		}
	}

	// Report current consumption.
	metrics.totalRU.Update(consumption.RU)
	metrics.totalReadRequests.Update(int64(consumption.ReadRequests))
	metrics.totalReadBytes.Update(int64(consumption.ReadBytes))
	metrics.totalWriteRequests.Update(int64(consumption.WriteRequests))
	metrics.totalWriteBytes.Update(int64(consumption.WriteBytes))
	metrics.totalSQLPodsCPUSeconds.Update(consumption.SQLPodsCPUSeconds)
	return result
}
