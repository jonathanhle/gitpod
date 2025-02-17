// Copyright (c) 2022 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package apiv1

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/gitpod-io/gitpod/usage/pkg/contentservice"

	"github.com/gitpod-io/gitpod/common-go/baseserver"
	v1 "github.com/gitpod-io/gitpod/usage-api/v1"
	"github.com/gitpod-io/gitpod/usage/pkg/db"
	"github.com/gitpod-io/gitpod/usage/pkg/db/dbtest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestUsageService_ListBilledUsage(t *testing.T) {
	ctx := context.Background()

	attributionID := db.NewTeamAttributionID(uuid.New().String())

	type Expectation struct {
		Code        codes.Code
		InstanceIds []string
	}

	type Scenario struct {
		name      string
		Instances []db.WorkspaceInstanceUsage
		Request   *v1.ListBilledUsageRequest
		Expect    Expectation
	}

	scenarios := []Scenario{
		{
			name:      "fails when From is after To",
			Instances: nil,
			Request: &v1.ListBilledUsageRequest{
				AttributionId: string(attributionID),
				From:          timestamppb.New(time.Date(2022, 07, 1, 13, 0, 0, 0, time.UTC)),
				To:            timestamppb.New(time.Date(2022, 07, 1, 12, 0, 0, 0, time.UTC)),
			},
			Expect: Expectation{
				Code:        codes.InvalidArgument,
				InstanceIds: nil,
			},
		},
		{
			name:      "fails when time range is greater than 31 days",
			Instances: nil,
			Request: &v1.ListBilledUsageRequest{
				AttributionId: string(attributionID),
				From:          timestamppb.New(time.Date(2022, 7, 1, 13, 0, 0, 0, time.UTC)),
				To:            timestamppb.New(time.Date(2022, 8, 1, 13, 0, 1, 0, time.UTC)),
			},
			Expect: Expectation{
				Code:        codes.InvalidArgument,
				InstanceIds: nil,
			},
		},
		(func() Scenario {
			start := time.Date(2022, 07, 1, 13, 0, 0, 0, time.UTC)
			attrID := db.NewTeamAttributionID(uuid.New().String())
			var instances []db.WorkspaceInstanceUsage
			for i := 0; i < 4; i++ {
				instance := dbtest.NewWorkspaceInstanceUsage(t, db.WorkspaceInstanceUsage{
					AttributionID: attrID,
					StartedAt:     start.Add(time.Duration(i) * 24 * time.Hour),
					StoppedAt: sql.NullTime{
						Time:  start.Add(time.Duration(i)*24*time.Hour + time.Hour),
						Valid: true,
					},
				})
				instances = append(instances, instance)
			}

			return Scenario{
				name:      "filters results to specified time range, ascending",
				Instances: instances,
				Request: &v1.ListBilledUsageRequest{
					AttributionId: string(attrID),
					From:          timestamppb.New(start),
					To:            timestamppb.New(start.Add(3 * 24 * time.Hour)),
					Order:         v1.ListBilledUsageRequest_ORDERING_ASCENDING,
				},
				Expect: Expectation{
					Code:        codes.OK,
					InstanceIds: []string{instances[0].InstanceID.String(), instances[1].InstanceID.String(), instances[2].InstanceID.String()},
				},
			}
		})(),
		(func() Scenario {
			start := time.Date(2022, 07, 1, 13, 0, 0, 0, time.UTC)
			attrID := db.NewTeamAttributionID(uuid.New().String())
			var instances []db.WorkspaceInstanceUsage
			for i := 0; i < 3; i++ {
				instance := dbtest.NewWorkspaceInstanceUsage(t, db.WorkspaceInstanceUsage{
					AttributionID: attrID,
					StartedAt:     start.Add(time.Duration(i) * 24 * time.Hour),
					StoppedAt: sql.NullTime{
						Time:  start.Add(time.Duration(i)*24*time.Hour + time.Hour),
						Valid: true,
					},
				})
				instances = append(instances, instance)
			}

			return Scenario{
				name:      "filters results to specified time range, descending",
				Instances: instances,
				Request: &v1.ListBilledUsageRequest{
					AttributionId: string(attrID),
					From:          timestamppb.New(start),
					To:            timestamppb.New(start.Add(5 * 24 * time.Hour)),
					Order:         v1.ListBilledUsageRequest_ORDERING_DESCENDING,
				},
				Expect: Expectation{
					Code:        codes.OK,
					InstanceIds: []string{instances[2].InstanceID.String(), instances[1].InstanceID.String(), instances[0].InstanceID.String()},
				},
			}
		})(),
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			dbconn := dbtest.ConnectForTests(t)
			dbtest.CreateWorkspaceInstanceUsageRecords(t, dbconn, scenario.Instances...)

			srv := baseserver.NewForTests(t,
				baseserver.WithGRPC(baseserver.MustUseRandomLocalAddress(t)),
			)

			generator := NewReportGenerator(dbconn, DefaultWorkspacePricer)
			v1.RegisterUsageServiceServer(srv.GRPC(), NewUsageService(dbconn, generator, nil, DefaultWorkspacePricer))
			baseserver.StartServerForTests(t, srv)

			conn, err := grpc.Dial(srv.GRPCAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)

			client := v1.NewUsageServiceClient(conn)

			resp, err := client.ListBilledUsage(ctx, scenario.Request)
			require.Equal(t, scenario.Expect.Code, status.Code(err))

			if err != nil {
				return
			}

			var instanceIds []string
			for _, billedSession := range resp.Sessions {
				instanceIds = append(instanceIds, billedSession.InstanceId)
			}

			require.Equal(t, scenario.Expect.InstanceIds, instanceIds)
		})
	}
}

func TestUsageService_ListBilledUsage_Pagination(t *testing.T) {
	ctx := context.Background()

	type Expectation struct {
		total      int64
		totalPages int64
	}

	type Scenario struct {
		name    string
		Request *v1.ListBilledUsageRequest
		Expect  Expectation
	}

	start := time.Date(2022, 07, 1, 13, 0, 0, 0, time.UTC)
	attrID := db.NewTeamAttributionID(uuid.New().String())
	var instances []db.WorkspaceInstanceUsage
	for i := 1; i <= 14; i++ {
		instance := dbtest.NewWorkspaceInstanceUsage(t, db.WorkspaceInstanceUsage{
			AttributionID: attrID,
			StartedAt:     start.Add(time.Duration(i) * time.Minute),
			StoppedAt: sql.NullTime{
				Time:  start.Add(time.Duration(i)*time.Minute + time.Hour),
				Valid: true,
			},
		})
		instances = append(instances, instance)
	}

	scenarios := []Scenario{
		{
			name: "first page",
			Request: &v1.ListBilledUsageRequest{
				AttributionId: string(attrID),
				From:          timestamppb.New(start),
				To:            timestamppb.New(start.Add(20*time.Minute + 2*time.Hour)),
				Pagination: &v1.PaginatedRequest{
					PerPage: int64(5),
					Page:    int64(1),
				},
			},
			Expect: Expectation{
				total:      int64(14),
				totalPages: int64(3),
			},
		},
		{
			name: "second page",
			Request: &v1.ListBilledUsageRequest{
				AttributionId: string(attrID),
				From:          timestamppb.New(start),
				To:            timestamppb.New(start.Add(20*time.Minute + 2*time.Hour)),
				Pagination: &v1.PaginatedRequest{
					PerPage: int64(5),
					Page:    int64(2),
				},
			},
			Expect: Expectation{
				total:      int64(14),
				totalPages: int64(3),
			},
		},
		{
			name: "third page",
			Request: &v1.ListBilledUsageRequest{
				AttributionId: string(attrID),
				From:          timestamppb.New(start),
				To:            timestamppb.New(start.Add(20*time.Minute + 2*time.Hour)),
				Pagination: &v1.PaginatedRequest{
					PerPage: int64(5),
					Page:    int64(3),
				},
			},
			Expect: Expectation{
				total:      int64(14),
				totalPages: int64(3),
			},
		},
		{
			name: "fourth page",
			Request: &v1.ListBilledUsageRequest{
				AttributionId: string(attrID),
				From:          timestamppb.New(start),
				To:            timestamppb.New(start.Add(20*time.Minute + 2*time.Hour)),
				Pagination: &v1.PaginatedRequest{
					PerPage: int64(5),
					Page:    int64(4),
				},
			},
			Expect: Expectation{
				total:      int64(14),
				totalPages: int64(3),
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			dbconn := dbtest.ConnectForTests(t)
			dbtest.CreateWorkspaceInstanceUsageRecords(t, dbconn, instances...)

			srv := baseserver.NewForTests(t,
				baseserver.WithGRPC(baseserver.MustUseRandomLocalAddress(t)),
			)

			generator := NewReportGenerator(dbconn, DefaultWorkspacePricer)
			v1.RegisterUsageServiceServer(srv.GRPC(), NewUsageService(dbconn, generator, nil, DefaultWorkspacePricer))
			baseserver.StartServerForTests(t, srv)

			conn, err := grpc.Dial(srv.GRPCAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)

			client := v1.NewUsageServiceClient(conn)

			resp, err := client.ListBilledUsage(ctx, scenario.Request)
			require.NoError(t, err)
			require.NotNil(t, resp.Pagination)

			require.Equal(t, scenario.Expect.total, resp.Pagination.Total)
			require.Equal(t, scenario.Expect.totalPages, resp.Pagination.TotalPages)
		})
	}
}

func TestInstanceToUsageRecords(t *testing.T) {
	maxStopTime := time.Date(2022, 05, 31, 23, 00, 00, 00, time.UTC)
	teamID, ownerID, projectID := uuid.New().String(), uuid.New(), uuid.New()
	workspaceID := dbtest.GenerateWorkspaceID()
	teamAttributionID := db.NewTeamAttributionID(teamID)
	instanceId := uuid.New()
	startedTime := db.NewVarcharTime(time.Date(2022, 05, 30, 00, 01, 00, 00, time.UTC))
	stoppingTime := db.NewVarcharTime(time.Date(2022, 06, 1, 1, 0, 0, 0, time.UTC))

	scenarios := []struct {
		Name     string
		Records  []db.WorkspaceInstanceForUsage
		Expected []db.WorkspaceInstanceUsage
	}{
		{
			Name: "a stopped workspace instance",
			Records: []db.WorkspaceInstanceForUsage{
				{
					ID:                 instanceId,
					WorkspaceID:        workspaceID,
					OwnerID:            ownerID,
					ProjectID:          sql.NullString{},
					WorkspaceClass:     defaultWorkspaceClass,
					Type:               db.WorkspaceType_Prebuild,
					UsageAttributionID: teamAttributionID,
					StartedTime:        startedTime,
					StoppingTime:       stoppingTime,
				},
			},
			Expected: []db.WorkspaceInstanceUsage{{
				InstanceID:     instanceId,
				AttributionID:  teamAttributionID,
				UserID:         ownerID,
				WorkspaceID:    workspaceID,
				ProjectID:      "",
				WorkspaceType:  db.WorkspaceType_Prebuild,
				WorkspaceClass: defaultWorkspaceClass,
				CreditsUsed:    469.8333333333333,
				StartedAt:      startedTime.Time(),
				StoppedAt:      sql.NullTime{Time: stoppingTime.Time(), Valid: true},
				GenerationID:   0,
				Deleted:        false,
			}},
		},
		{
			Name: "workspace instance that is still running",
			Records: []db.WorkspaceInstanceForUsage{
				{
					ID:                 instanceId,
					OwnerID:            ownerID,
					ProjectID:          sql.NullString{String: projectID.String(), Valid: true},
					WorkspaceClass:     defaultWorkspaceClass,
					Type:               db.WorkspaceType_Regular,
					WorkspaceID:        workspaceID,
					UsageAttributionID: teamAttributionID,
					StartedTime:        startedTime,
					StoppingTime:       db.VarcharTime{},
				},
			},
			Expected: []db.WorkspaceInstanceUsage{{
				InstanceID:     instanceId,
				AttributionID:  teamAttributionID,
				UserID:         ownerID,
				ProjectID:      projectID.String(),
				WorkspaceID:    workspaceID,
				WorkspaceType:  db.WorkspaceType_Regular,
				StartedAt:      startedTime.Time(),
				StoppedAt:      sql.NullTime{},
				WorkspaceClass: defaultWorkspaceClass,
				CreditsUsed:    469.8333333333333,
			}},
		},
	}

	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			actual := instancesToUsageRecords(s.Records, DefaultWorkspacePricer, maxStopTime)
			require.Equal(t, s.Expected, actual)
		})
	}
}

func TestReportGenerator_GenerateUsageReport(t *testing.T) {
	startOfMay := time.Date(2022, 05, 1, 0, 00, 00, 00, time.UTC)
	startOfJune := time.Date(2022, 06, 1, 0, 00, 00, 00, time.UTC)

	teamID := uuid.New()
	scenarioRunTime := time.Date(2022, 05, 31, 23, 00, 00, 00, time.UTC)

	instances := []db.WorkspaceInstance{
		// Ran throughout the reconcile period
		dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
			ID:                 uuid.New(),
			UsageAttributionID: db.NewTeamAttributionID(teamID.String()),
			StartedTime:        db.NewVarcharTime(time.Date(2022, 05, 1, 00, 01, 00, 00, time.UTC)),
			StoppingTime:       db.NewVarcharTime(time.Date(2022, 06, 1, 1, 0, 0, 0, time.UTC)),
		}),
		// Still running
		dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
			ID:                 uuid.New(),
			UsageAttributionID: db.NewTeamAttributionID(teamID.String()),
			StartedTime:        db.NewVarcharTime(time.Date(2022, 05, 30, 00, 01, 00, 00, time.UTC)),
		}),
		// No creation time, invalid record, ignored
		dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
			ID:                 uuid.New(),
			UsageAttributionID: db.NewTeamAttributionID(teamID.String()),
			StoppingTime:       db.NewVarcharTime(time.Date(2022, 06, 1, 1, 0, 0, 0, time.UTC)),
		}),
	}

	conn := dbtest.ConnectForTests(t)
	dbtest.CreateWorkspaceInstances(t, conn, instances...)

	nowFunc := func() time.Time { return scenarioRunTime }
	generator := &ReportGenerator{
		nowFunc: nowFunc,
		conn:    conn,
		pricer:  DefaultWorkspacePricer,
	}

	report, err := generator.GenerateUsageReport(context.Background(), startOfMay, startOfJune)
	require.NoError(t, err)

	require.Equal(t, nowFunc(), report.GenerationTime)
	require.Equal(t, startOfMay, report.From)
	// require.Equal(t, startOfJune, report.To) TODO(gpl) This is not true anymore - does it really make sense to test for it?
	require.Len(t, report.InvalidSessions, 0)
	require.Len(t, report.UsageRecords, 2)
}

func TestReportGenerator_GenerateUsageReportTable(t *testing.T) {
	teamID := uuid.New()
	instanceID := uuid.New()

	Must := func(ti db.VarcharTime, err error) db.VarcharTime {
		if err != nil {
			t.Fatal(err)
		}
		return ti
	}
	Timestamp := func(timestampAsStr string) db.VarcharTime {
		return Must(db.NewVarcharTimeFromStr(timestampAsStr))
	}
	type Expectation struct {
		custom       func(t *testing.T, report contentservice.UsageReport)
		usageRecords []db.WorkspaceInstanceUsage
	}

	type TestCase struct {
		name        string
		from        time.Time
		to          time.Time
		runtime     time.Time
		instances   []db.WorkspaceInstance
		expectation Expectation
	}
	tests := []TestCase{
		{
			name:    "real example taken from DB: runtime _before_ instance.startedTime",
			from:    time.Date(2022, 8, 1, 0, 00, 00, 00, time.UTC),
			to:      time.Date(2022, 9, 1, 0, 00, 00, 00, time.UTC),
			runtime: Timestamp("2022-08-17T09:38:28Z").Time(),
			instances: []db.WorkspaceInstance{
				dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
					ID:                 instanceID,
					UsageAttributionID: db.NewTeamAttributionID(teamID.String()),
					CreationTime:       Timestamp("2022-08-17T09:40:47.316Z"),
					StartedTime:        Timestamp("2022-08-17T09:40:53.115Z"),
					StoppingTime:       Timestamp("2022-08-17T09:42:36.292Z"),
					StoppedTime:        Timestamp("2022-08-17T09:43:04.874Z"),
				}),
			},
			expectation: Expectation{
				usageRecords: nil,
				// usageRecords: []db.WorkspaceInstanceUsage{
				// 	{
				// 		InstanceID: instanceID,
				// 		AttributionID: db.NewTeamAttributionID(teamID.String()),
				// 		StartedAt: Timestamp("2022-08-17T09:40:53.115Z").Time(),
				// 		StoppedAt: sql.NullTime{ Time: Timestamp("2022-08-17T09:43:04.874Z").Time(), Valid: true },
				// 		WorkspaceClass: "default",
				// 		CreditsUsed: 3.0,
				// 	},
				// },
			},
		},
		{
			name:    "same as above, but with runtime _after_ startedTime",
			from:    time.Date(2022, 8, 1, 0, 00, 00, 00, time.UTC),
			to:      time.Date(2022, 9, 1, 0, 00, 00, 00, time.UTC),
			runtime: Timestamp("2022-08-17T09:41:00Z").Time(),
			instances: []db.WorkspaceInstance{
				dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
					ID:                 instanceID,
					UsageAttributionID: db.NewTeamAttributionID(teamID.String()),
					CreationTime:       Timestamp("2022-08-17T09:40:47.316Z"),
					StartedTime:        Timestamp("2022-08-17T09:40:53.115Z"),
					StoppingTime:       Timestamp("2022-08-17T09:42:36.292Z"),
					StoppedTime:        Timestamp("2022-08-17T09:43:04.874Z"),
				}),
			},
			expectation: Expectation{
				usageRecords: []db.WorkspaceInstanceUsage{
					{
						InstanceID:     instanceID,
						AttributionID:  db.NewTeamAttributionID(teamID.String()),
						StartedAt:      Timestamp("2022-08-17T09:40:53.115Z").Time(),
						StoppedAt:      sql.NullTime{Time: Timestamp("2022-08-17T09:41:00Z").Time(), Valid: true},
						WorkspaceClass: "default",
						CreditsUsed:    0.019444444444444445,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := dbtest.ConnectForTests(t)
			dbtest.CreateWorkspaceInstances(t, conn, test.instances...)

			nowFunc := func() time.Time { return test.runtime }
			generator := &ReportGenerator{
				nowFunc: nowFunc,
				conn:    conn,
				pricer:  DefaultWorkspacePricer,
			}

			report, err := generator.GenerateUsageReport(context.Background(), test.from, test.to)
			require.NoError(t, err)

			require.Equal(t, test.runtime, report.GenerationTime)
			require.Equal(t, test.from, report.From)
			// require.Equal(t, test.to, report.To) TODO(gpl) This is not true anymore - does it really make sense to test for it?

			// These invariants should always be true:
			// 1. No negative usage
			for _, rec := range report.UsageRecords {
				if rec.CreditsUsed < 0 {
					t.Error("Got report with negative credits!")
				}
			}

			if !reflect.DeepEqual(test.expectation.usageRecords, report.UsageRecords) {
				t.Errorf("report.UsageRecords: expected %v but got %v", test.expectation.usageRecords, report.UsageRecords)
			}

			// Custom expectations
			customTestFunction := test.expectation.custom
			if customTestFunction != nil {
				customTestFunction(t, report)
				require.NoError(t, err)
			}
		})
	}
}

func TestUsageService_ReconcileUsageWithLedger(t *testing.T) {
	dbconn := dbtest.ConnectForTests(t)
	from := time.Date(2022, 05, 1, 0, 00, 00, 00, time.UTC)
	to := time.Date(2022, 05, 1, 1, 00, 00, 00, time.UTC)
	attributionID := db.NewTeamAttributionID(uuid.New().String())

	t.Cleanup(func() {
		require.NoError(t, dbconn.Where("attributionId = ?", attributionID).Delete(&db.Usage{}).Error)
	})

	// stopped instances
	instance := dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
		UsageAttributionID: attributionID,
		CreationTime:       db.NewVarcharTime(from),
		StoppingTime:       db.NewVarcharTime(to.Add(-1 * time.Minute)),
	})
	dbtest.CreateWorkspaceInstances(t, dbconn, instance)

	// running instances
	dbtest.CreateWorkspaceInstances(t, dbconn, dbtest.NewWorkspaceInstance(t, db.WorkspaceInstance{
		UsageAttributionID: attributionID,
	}))

	// usage drafts
	dbtest.CreateUsageRecords(t, dbconn, dbtest.NewUsage(t, db.Usage{
		ID:                  uuid.New(),
		AttributionID:       attributionID,
		WorkspaceInstanceID: instance.ID,
		Kind:                db.WorkspaceInstanceUsageKind,
		Draft:               true,
	}))

	srv := baseserver.NewForTests(t,
		baseserver.WithGRPC(baseserver.MustUseRandomLocalAddress(t)),
	)

	v1.RegisterUsageServiceServer(srv.GRPC(), NewUsageService(dbconn, nil, nil, DefaultWorkspacePricer))
	baseserver.StartServerForTests(t, srv)

	conn, err := grpc.Dial(srv.GRPCAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	client := v1.NewUsageServiceClient(conn)

	_, err = client.ReconcileUsageWithLedger(context.Background(), &v1.ReconcileUsageWithLedgerRequest{
		From: timestamppb.New(from),
		To:   timestamppb.New(to),
	})
	require.NoError(t, err)

	usage, err := db.FindUsage(context.Background(), dbconn, &db.FindUsageParams{
		AttributionId: attributionID,
		From:          from,
		To:            to,
		ExcludeDrafts: false,
	})
	require.NoError(t, err)
	require.Len(t, usage, 1)
}

func TestReconcileWithLedger(t *testing.T) {
	now := time.Date(2022, 9, 1, 10, 0, 0, 0, time.UTC)
	pricer, err := NewWorkspacePricer(map[string]float64{
		"default":              0.1666666667,
		"g1-standard":          0.1666666667,
		"g1-standard-pvc":      0.1666666667,
		"g1-large":             0.3333333333,
		"g1-large-pvc":         0.3333333333,
		"gitpodio-internal-xl": 0.3333333333,
	})
	require.NoError(t, err)

	t.Run("no action with no instances and no drafts", func(t *testing.T) {
		inserts, updates, err := reconcileUsageWithLedger(nil, nil, pricer, now)
		require.NoError(t, err)
		require.Len(t, inserts, 0)
		require.Len(t, updates, 0)
	})

	t.Run("no action with no instances but existing drafts", func(t *testing.T) {
		drafts := []db.Usage{dbtest.NewUsage(t, db.Usage{})}
		inserts, updates, err := reconcileUsageWithLedger(nil, drafts, pricer, now)
		require.NoError(t, err)
		require.Len(t, inserts, 0)
		require.Len(t, updates, 0)
	})

	t.Run("creates a new usage record when no draft exists, removing duplicates", func(t *testing.T) {
		instance := db.WorkspaceInstanceForUsage{
			ID:          uuid.New(),
			WorkspaceID: dbtest.GenerateWorkspaceID(),
			OwnerID:     uuid.New(),
			ProjectID: sql.NullString{
				String: "my-project",
				Valid:  true,
			},
			WorkspaceClass:     db.WorkspaceClass_Default,
			Type:               db.WorkspaceType_Regular,
			UsageAttributionID: db.NewTeamAttributionID(uuid.New().String()),
			StartedTime:        db.NewVarcharTime(now.Add(1 * time.Minute)),
		}

		inserts, updates, err := reconcileUsageWithLedger([]db.WorkspaceInstanceForUsage{instance, instance}, nil, pricer, now)
		require.NoError(t, err)
		require.Len(t, inserts, 1)
		require.Len(t, updates, 0)
		expectedUsage := db.Usage{
			ID:                  inserts[0].ID,
			AttributionID:       instance.UsageAttributionID,
			Description:         usageDescriptionFromController,
			CreditCents:         db.NewCreditCents(pricer.CreditsUsedByInstance(&instance, now)),
			EffectiveTime:       db.NewVarcharTime(now),
			Kind:                db.WorkspaceInstanceUsageKind,
			WorkspaceInstanceID: instance.ID,
			Draft:               true,
			Metadata:            nil,
		}
		require.NoError(t, expectedUsage.SetMetadataWithWorkspaceInstance(db.WorkspaceInstanceUsageData{
			WorkspaceId:    instance.WorkspaceID,
			WorkspaceType:  instance.Type,
			WorkspaceClass: instance.WorkspaceClass,
			ContextURL:     "",
			StartTime:      db.TimeToISO8601(instance.StartedTime.Time()),
			EndTime:        "",
			UserName:       "",
			UserAvatarURL:  "",
		}))
		require.EqualValues(t, expectedUsage, inserts[0])
	})

	t.Run("updates a usage record when a draft exists", func(t *testing.T) {
		instance := db.WorkspaceInstanceForUsage{
			ID:          uuid.New(),
			WorkspaceID: dbtest.GenerateWorkspaceID(),
			OwnerID:     uuid.New(),
			ProjectID: sql.NullString{
				String: "my-project",
				Valid:  true,
			},
			WorkspaceClass:     db.WorkspaceClass_Default,
			Type:               db.WorkspaceType_Regular,
			UsageAttributionID: db.NewTeamAttributionID(uuid.New().String()),
			StartedTime:        db.NewVarcharTime(now.Add(1 * time.Minute)),
		}

		// the fields in the usage record deliberately do not match the instance, except for the Instance ID.
		// we do this to test that the fields in the usage records get updated to reflect the true values from the source of truth - instances.
		draft := dbtest.NewUsage(t, db.Usage{
			ID:                  uuid.New(),
			AttributionID:       db.NewUserAttributionID(uuid.New().String()),
			Description:         "Some description",
			CreditCents:         1,
			EffectiveTime:       db.VarcharTime{},
			Kind:                db.WorkspaceInstanceUsageKind,
			WorkspaceInstanceID: instance.ID,
			Draft:               true,
			Metadata:            nil,
		})

		inserts, updates, err := reconcileUsageWithLedger([]db.WorkspaceInstanceForUsage{instance}, []db.Usage{draft}, pricer, now)
		require.NoError(t, err)
		require.Len(t, inserts, 0)
		require.Len(t, updates, 1)

		expectedUsage := db.Usage{
			ID:                  draft.ID,
			AttributionID:       instance.UsageAttributionID,
			Description:         usageDescriptionFromController,
			CreditCents:         db.NewCreditCents(pricer.CreditsUsedByInstance(&instance, now)),
			EffectiveTime:       db.NewVarcharTime(now),
			Kind:                db.WorkspaceInstanceUsageKind,
			WorkspaceInstanceID: instance.ID,
			Draft:               true,
			Metadata:            nil,
		}
		require.NoError(t, expectedUsage.SetMetadataWithWorkspaceInstance(db.WorkspaceInstanceUsageData{
			WorkspaceId:    instance.WorkspaceID,
			WorkspaceType:  instance.Type,
			WorkspaceClass: instance.WorkspaceClass,
			ContextURL:     "",
			StartTime:      db.TimeToISO8601(instance.StartedTime.Time()),
			EndTime:        "",
			UserName:       "",
			UserAvatarURL:  "",
		}))
		require.EqualValues(t, expectedUsage, updates[0])
	})
}
