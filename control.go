package cke

import (
	"context"
	"os"

	"time"

	"github.com/coreos/etcd/clientv3/concurrency"
)

// Controller manage operations
type Controller struct {
	session  *concurrency.Session
	interval time.Duration
}

// Run execute procedures with leader elections
func (c Controller) Run(ctx context.Context) error {
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	e := concurrency.NewElection(c.session, KeyLeader)

RETRY:
	err = e.Campaign(ctx, hostname)
	if err != nil {
		return err
	}
	leaderKey := e.Key()

	err = c.runLoop(ctx, leaderKey)
	if err == ErrNoLeader {
		err2 := e.Resign(ctx)
		if err2 != nil {
			return err2
		}
		goto RETRY
	}
	return err
}

func (c Controller) runLoop(ctx context.Context, leaderKey string) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		err := c.runOnce(ctx, leaderKey, ticker.C)
		if err != nil {
			return err
		}
	}
}

func (c Controller) runOnce(ctx context.Context, leaderKey string, tick <-chan time.Time) error {
	storage := Storage{c.session.Client()}
	cluster, err := storage.GetCluster(ctx)
	if err != nil {
		return err
	}

	status, err := GetClusterStatus(ctx, cluster)
	if err != nil {
		return err
	}

	op := DecideToDo(cluster, status)
	if op == nil {
		select {
		case <-ctx.Done():
		case <-tick:
		}
		return nil
	}

	// register operation record
	id, err := storage.NextRecordID(ctx)
	if err != nil {
		return err
	}
	record := op.NewRecord(id)
	err = storage.RegisterRecord(ctx, leaderKey, record)
	if err != nil {
		return err
	}

	err = op.Cleanup(ctx)
	if err != nil {
		return err
	}

	for {
		commander := op.NextCommand()
		if commander == nil {
			break
		}
		record.SetCommand(commander.Command())
		err = storage.UpdateRecord(ctx, leaderKey, record)
		if err != nil {
			return err
		}
		err = commander.Run(ctx)
		if err == nil {
			continue
		}

		record.SetError(err)
		storage.UpdateRecord(ctx, leaderKey, record)
		return err
	}

	record.Complete()
	return storage.UpdateRecord(ctx, leaderKey, record)
}
