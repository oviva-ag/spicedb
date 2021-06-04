package crdb

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/authzed/spicedb/internal/datastore"
	pb "github.com/authzed/spicedb/pkg/REDACTEDapi/api"
	"github.com/shopspring/decimal"
)

const queryChangefeed = "EXPERIMENTAL CHANGEFEED FOR %s WITH updated, cursor = '%s', resolved = '1s';"

func (cds *crdbDatastore) Watch(ctx context.Context, afterRevision datastore.Revision) (<-chan *datastore.RevisionChanges, <-chan error) {
	updates := make(chan *datastore.RevisionChanges, cds.watchBufferLength)
	errors := make(chan error, 1)

	interpolated := fmt.Sprintf(queryChangefeed, tableTuple, afterRevision)

	go func() {
		defer close(updates)
		defer close(errors)

		pendingChanges := make(map[decimal.Decimal][]*pb.RelationTupleUpdate)

		changes, err := cds.conn.Query(ctx, interpolated)
		if err != nil {
			if ctx.Err() == context.Canceled {
				errors <- datastore.ErrWatchCanceled
			} else {
				errors <- err
			}
			return
		}

		// We call Close async here because it can be slow and blocks closing the channels. There is
		// no return value so we're not really losing anything.
		defer func() { go changes.Close() }()

		for changes.Next() {
			var unused interface{}
			var changeJson []byte
			var primaryKeyValuesJson []byte

			if err := changes.Scan(&unused, &primaryKeyValuesJson, &changeJson); err != nil {
				if ctx.Err() == context.Canceled {
					errors <- datastore.ErrWatchCanceled
				} else {
					errors <- err
				}
				return
			}

			var changeDetails struct {
				Resolved string
				Updated  string
				After    interface{}
			}
			if err := json.Unmarshal(changeJson, &changeDetails); err != nil {
				errors <- err
				return
			}

			if changeDetails.Resolved != "" {
				// This entry indicates that we are ready to potentially emit some changes
				resolved, err := decimal.NewFromString(changeDetails.Resolved)
				if err != nil {
					errors <- err
					return
				}

				var toEmit []*datastore.RevisionChanges
				for ts, values := range pendingChanges {
					if ts.LessThanOrEqual(resolved) {
						delete(pendingChanges, ts)

						toEmit = append(toEmit, &datastore.RevisionChanges{
							Revision: ts,
							Changes:  values,
						})
					}
				}

				sort.Slice(toEmit, func(i, j int) bool {
					return toEmit[i].Revision.LessThan(toEmit[j].Revision)
				})

				for _, change := range toEmit {
					select {
					case updates <- change:
					default:
						errors <- datastore.ErrWatchDisconnected
						return
					}
				}

				continue
			}

			var pkValues [6]string
			if err := json.Unmarshal(primaryKeyValuesJson, &pkValues); err != nil {
				errors <- err
				return
			}

			revision, err := decimal.NewFromString(changeDetails.Updated)
			if err != nil {
				errors <- fmt.Errorf("malformed update timestamp: %w", err)
				return
			}

			oneChange := &pb.RelationTupleUpdate{
				Tuple: &pb.RelationTuple{
					ObjectAndRelation: &pb.ObjectAndRelation{
						Namespace: pkValues[0],
						ObjectId:  pkValues[1],
						Relation:  pkValues[2],
					},
					User: &pb.User{
						UserOneof: &pb.User_Userset{
							Userset: &pb.ObjectAndRelation{
								Namespace: pkValues[3],
								ObjectId:  pkValues[4],
								Relation:  pkValues[5],
							},
						},
					},
				},
			}
			if changeDetails.After == nil {
				oneChange.Operation = pb.RelationTupleUpdate_DELETE
			} else {
				oneChange.Operation = pb.RelationTupleUpdate_TOUCH
			}

			pendingChanges[revision] = append(pendingChanges[revision], oneChange)
		}
		if changes.Err() != nil {
			if ctx.Err() == context.Canceled {
				errors <- datastore.ErrWatchCanceled
			} else {
				errors <- changes.Err()
			}
			return
		}
	}()
	return updates, errors
}
