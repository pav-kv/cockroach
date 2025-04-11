// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package keys

type tagNode struct {
	atom     []byte
	children []tagNode
}

type tag []byte

func (t tag) sub(nodes ...tagNode) tagNode {
	return tagNode{
		atom:     t,
		children: nodes,
	}
}

// LocalMax
var _ = tag(nil).sub(
	tag(LocalPrefix).sub(
		// add rangeID
		tag(LocalRangeIDPrefix).sub(
			tag(LocalRangeIDReplicatedInfix).sub(
				tag(LocalAbortSpanSuffix).sub(),
				tag(LocalReplicatedSharedLocksTransactionLatchingKeySuffix).sub(),
				tag(localRangeFrozenStatusSuffix).sub(),
				tag(LocalRangeGCThresholdSuffix).sub(),
				tag(LocalRangeAppliedStateSuffix).sub(),
				tag(LocalRangeForceFlushSuffix).sub(),
				tag(LocalRangeGCHintSuffix).sub(),
				tag(LocalRangeLeaseSuffix).sub(),
				tag(LocalRangePriorReadSummarySuffix).sub(),
				tag(LocalRangeVersionSuffix).sub(),
				tag(LocalRangeStatsLegacySuffix).sub(),
				tag(localTxnSpanGCThresholdSuffix).sub(), // deprecated
			),

			tag(localRangeIDUnreplicatedInfix).sub(
				tag(LocalRangeTombstoneSuffix).sub(),
				tag(LocalRaftHardStateSuffix).sub(),
				tag(localRaftLastIndexSuffix).sub(),
				tag(LocalRaftLogSuffix).sub(),
				tag(LocalRaftReplicaIDSuffix).sub(),
				tag(LocalRaftTruncatedStateSuffix).sub(),
			),
		),

		tag(LocalRangePrefix).sub(
			tag(LocalRangeProbeSuffix).sub(),
			tag(LocalQueueLastProcessedSuffix).sub(),
			tag(LocalRangeDescriptorSuffix).sub(),
			tag(LocalTransactionSuffix).sub(),
		),

		tag(LocalStorePrefix).sub(
			tag(localStoreClusterVersionSuffix).sub(),
			tag(localStoreGossipSuffix).sub(),
			tag(localStoreHLCUpperBoundSuffix).sub(),
			tag(localStoreIdentSuffix).sub(),

			tag(localStoreLossOfQuorumRecoveryInfix).sub(
				tag(localStoreUnsafeReplicaRecoverySuffix).sub(),
				// LocalStoreUnsafeReplicaRecoveryKeyMin,
				// LocalStoreUnsafeReplicaRecoveryKeyMax,
				tag(localStoreLossOfQuorumRecoveryStatusSuffix).sub(),
				tag(localStoreLossOfQuorumRecoveryCleanupActionsSuffix).sub(),
			),

			tag(localStoreNodeTombstoneSuffix).sub(),
			// LocalStoreCachedSettingsKeyMin,
			// LocalStoreCachedSettingsKeyMax,
			tag(localStoreCachedSettingsSuffix).sub(),

			tag(localStoreLastUpSuffix).sub(),
			tag(localStoreLivenessRequesterMeta).sub(),
			tag(localStoreLivenessSupporterMeta).sub(),
			tag(localStoreLivenessSupportFor).sub(),
			tag(localRemovedLeakedRaftEntriesSuffix).sub(),
		),

		tag(LocalRangeLockTablePrefix).sub(
			// LockTableSingleKeyStart
			// LockTableSingleKeyEnd
			tag(LockTableSingleKeyInfix).sub(),
		),
	),

	// MetaMin = Meta1Prefix, MetaMax = metaMaxByte
	// Meta1KeyMax
	tag(Meta1Prefix).sub(),
	// Meta2KeyMax
	tag(Meta2Prefix).sub(),

	// SystemMax
	tag(SystemPrefix).sub(
		// NodeLivenessKeyMax
		tag(NodeLivenessPrefix).sub(),
		tag(BootstrapVersionKey).sub(),
		tag(ClusterInitGracePeriodTimestamp).sub(),
		tag(TrialLicenseExpiry).sub(),
		tag(NodeIDGenerator).sub(),
		tag(RangeIDGenerator).sub(),
		tag(StoreIDGenerator).sub(),
		tag(StatusPrefix).sub(),
		tag(StatusNodePrefix).sub(),
		tag(StartupMigrationPrefix).sub(),
		// TimeseriesKeyMax
		tag(TimeseriesPrefix).sub(),
		// SystemSpanConfigKeyMax
		tag(SystemSpanConfigPrefix).sub(
			tag(SystemSpanConfigEntireKeyspace).sub(),
			tag(SystemSpanConfigHostOnTenantKeyspace).sub(),
			tag(SystemSpanConfigSecondaryTenantOnEntireKeyspace).sub(),
		),
	),

	// TableDataMax
	tag(TableDataMin).sub(
	// TODO
	),

	// TenantTableDataMin/Max
	tag(TenantPrefix).sub(),
)
