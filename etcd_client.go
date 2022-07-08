/**
 * Standalone signaling server for the Nextcloud Spreed app.
 * Copyright (C) 2022 struktur AG
 *
 * @author Joachim Bauch <bauch@struktur.de>
 *
 * @license GNU AGPL version 3 or any later version
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */
package signaling

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlintw/goconf"
	"go.etcd.io/etcd/client/pkg/v3/srv"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type EtcdClientListener interface {
	EtcdClientCreated(client *EtcdClient)
}

type EtcdClientWatcher interface {
	EtcdWatchCreated(client *EtcdClient, key string)
	EtcdKeyUpdated(client *EtcdClient, key string, value []byte)
	EtcdKeyDeleted(client *EtcdClient, key string)
}

type EtcdClient struct {
	compatSection string

	mu        sync.Mutex
	client    atomic.Value
	listeners map[EtcdClientListener]bool
}

func NewEtcdClient(config *goconf.ConfigFile, compatSection string) (*EtcdClient, error) {
	result := &EtcdClient{
		compatSection: compatSection,
	}
	if err := result.load(config, false); err != nil {
		return nil, err
	}

	return result, nil
}

func (c *EtcdClient) getConfigStringWithFallback(config *goconf.ConfigFile, option string) string {
	value, _ := config.GetString("etcd", option)
	if value == "" && c.compatSection != "" {
		value, _ = config.GetString(c.compatSection, option)
		if value != "" {
			log.Printf("WARNING: Configuring etcd option \"%s\" in section \"%s\" is deprecated, use section \"etcd\" instead", option, c.compatSection)
		}
	}

	return value
}

func (c *EtcdClient) load(config *goconf.ConfigFile, ignoreErrors bool) error {
	var endpoints []string
	if endpointsString := c.getConfigStringWithFallback(config, "endpoints"); endpointsString != "" {
		for _, ep := range strings.Split(endpointsString, ",") {
			ep := strings.TrimSpace(ep)
			if ep != "" {
				endpoints = append(endpoints, ep)
			}
		}
	} else if discoverySrv := c.getConfigStringWithFallback(config, "discoverysrv"); discoverySrv != "" {
		discoveryService := c.getConfigStringWithFallback(config, "discoveryservice")
		clients, err := srv.GetClient("etcd-client", discoverySrv, discoveryService)
		if err != nil {
			if !ignoreErrors {
				return fmt.Errorf("Could not discover etcd endpoints for %s: %w", discoverySrv, err)
			}
		} else {
			endpoints = clients.Endpoints
		}
	}

	if len(endpoints) == 0 {
		if !ignoreErrors {
			return nil
		}

		log.Printf("No etcd endpoints configured, not changing client")
	} else {
		cfg := clientv3.Config{
			Endpoints: endpoints,

			// set timeout per request to fail fast when the target endpoint is unavailable
			DialTimeout: time.Second,
		}

		clientKey := c.getConfigStringWithFallback(config, "clientkey")
		clientCert := c.getConfigStringWithFallback(config, "clientcert")
		caCert := c.getConfigStringWithFallback(config, "cacert")
		if clientKey != "" && clientCert != "" && caCert != "" {
			tlsInfo := transport.TLSInfo{
				CertFile:      clientCert,
				KeyFile:       clientKey,
				TrustedCAFile: caCert,
			}
			tlsConfig, err := tlsInfo.ClientConfig()
			if err != nil {
				if !ignoreErrors {
					return fmt.Errorf("Could not setup etcd TLS configuration: %w", err)
				}

				log.Printf("Could not setup TLS configuration, will be disabled (%s)", err)
			} else {
				cfg.TLS = tlsConfig
			}
		}

		client, err := clientv3.New(cfg)
		if err != nil {
			if !ignoreErrors {
				return err
			}

			log.Printf("Could not create new client from etd endpoints %+v: %s", endpoints, err)
		} else {
			prev := c.getEtcdClient()
			if prev != nil {
				prev.Close()
			}
			c.client.Store(client)
			log.Printf("Using etcd endpoints %+v", endpoints)
			c.notifyListeners()
		}
	}

	return nil
}

func (c *EtcdClient) Close() error {
	client := c.getEtcdClient()
	if client != nil {
		return client.Close()
	}

	return nil
}

func (c *EtcdClient) IsConfigured() bool {
	return c.getEtcdClient() != nil
}

func (c *EtcdClient) getEtcdClient() *clientv3.Client {
	client := c.client.Load()
	if client == nil {
		return nil
	}

	return client.(*clientv3.Client)
}

func (c *EtcdClient) syncClient() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	return c.getEtcdClient().Sync(ctx)
}

func (c *EtcdClient) notifyListeners() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for listener := range c.listeners {
		listener.EtcdClientCreated(c)
	}
}

func (c *EtcdClient) AddListener(listener EtcdClientListener) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.listeners == nil {
		c.listeners = make(map[EtcdClientListener]bool)
	}
	c.listeners[listener] = true
	if client := c.getEtcdClient(); client != nil {
		go listener.EtcdClientCreated(c)
	}
}

func (c *EtcdClient) RemoveListener(listener EtcdClientListener) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.listeners, listener)
}

func (c *EtcdClient) WaitForConnection() {
	waitDelay := initialWaitDelay
	for {
		if err := c.syncClient(); err != nil {
			if err == context.DeadlineExceeded {
				log.Printf("Timeout waiting for etcd client to connect to the cluster, retry in %s", waitDelay)
			} else {
				log.Printf("Could not sync etcd client with the cluster, retry in %s: %s", waitDelay, err)
			}

			time.Sleep(waitDelay)
			waitDelay = waitDelay * 2
			if waitDelay > maxWaitDelay {
				waitDelay = maxWaitDelay
			}
			continue
		}

		log.Printf("Client synced, using endpoints %+v", c.getEtcdClient().Endpoints())
		return
	}
}

func (c *EtcdClient) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	return c.getEtcdClient().Get(ctx, key, opts...)
}

func (c *EtcdClient) Watch(ctx context.Context, key string, watcher EtcdClientWatcher, opts ...clientv3.OpOption) error {
	log.Printf("Wait for leader and start watching on %s", key)
	ch := c.getEtcdClient().Watch(clientv3.WithRequireLeader(ctx), key, opts...)
	log.Printf("Watch created for %s", key)
	watcher.EtcdWatchCreated(c, key)
	for response := range ch {
		if err := response.Err(); err != nil {
			return err
		}

		for _, ev := range response.Events {
			switch ev.Type {
			case clientv3.EventTypePut:
				watcher.EtcdKeyUpdated(c, string(ev.Kv.Key), ev.Kv.Value)
			case clientv3.EventTypeDelete:
				watcher.EtcdKeyDeleted(c, string(ev.Kv.Key))
			default:
				log.Printf("Unsupported watch event %s %q -> %q", ev.Type, ev.Kv.Key, ev.Kv.Value)
			}
		}
	}

	return nil
}
