package cluster

import (
	"strings"
	"testing"

	"github.com/deemwar-products/chlift/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{Cluster: "prod", ClickHouseVersion: "26.3", Hosts: []config.Host{
		{Name: "a", Address: "10.0.0.1", Roles: []string{"clickhouse", "keeper"}},
		{Name: "b", Address: "10.0.0.2", Roles: []string{"clickhouse", "keeper"}},
		{Name: "c", Address: "10.0.0.3", Roles: []string{"keeper"}},
	}}
}

func TestServerConfigHasQuorumAndReplicas(t *testing.T) {
	c := testConfig()
	got := string(render("server.xml", newRenderData(c, c.Hosts[1], 8<<30, "pw", "sec")))
	for _, want := range []string{
		"<listen_host>10.0.0.2</listen_host>", "<replica>b</replica>", "<server_id>2</server_id>",
		"<hostname>10.0.0.3</hostname>", "<host>10.0.0.1</host><port>9000</port>", "<secret>sec</secret>",
		"<port>9363</port>", "<max_server_memory_usage>6442450944</max_server_memory_usage>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("server.xml missing %q", want)
		}
	}
	if strings.Contains(got, "pw") {
		t.Error("server.xml must not contain the password")
	}
}

func TestUsersConfigHashesPassword(t *testing.T) {
	c := testConfig()
	got := string(render("users.xml", newRenderData(c, c.Hosts[0], 8<<30, "pw", "sec")))
	if strings.Contains(got, ">pw<") || !strings.Contains(got, "<password_sha256_hex>30c952fab122c3f9759f02a6d95c3758b246b4fee239957b2d4fee46e26170c4</password_sha256_hex>") {
		t.Errorf("users.xml must carry only the sha256 of the password:\n%s", got)
	}
	if !strings.Contains(got, "<database_replicated_allow_replicated_engine_arguments>2<") {
		t.Error("users.xml must allow PeerDB's explicit replicated engine args inside Replicated databases")
	}
}

func TestKeeperOnlyConfig(t *testing.T) {
	c := testConfig()
	got := string(render("keeper.xml", newRenderData(c, c.Hosts[2], 2<<30, "pw", "sec")))
	if !strings.Contains(got, "<server_id>3</server_id>") || strings.Count(got, "<server><id>") != 3 {
		t.Errorf("keeper.xml wrong:\n%s", got)
	}
}

func TestValidateRefusesTwoKeepers(t *testing.T) {
	c := testConfig()
	c.Hosts = c.Hosts[:2]
	errs, _ := c.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0], "2 keeper members") {
		t.Errorf("want a 2-keeper refusal, got %v", errs)
	}
}

func TestAllInterfacesDoesNotAlsoBindLoopback(t *testing.T) {
	// 0.0.0.0 already covers 127.0.0.1; listing both makes ClickHouse fail with "Address already in use".
	c := testConfig()
	c.Hosts[0].PrivateAddress = "node-a"
	for _, name := range []string{"server.xml", "keeper.xml"} {
		got := string(render(name, newRenderData(c, c.Hosts[0], 8<<30, "pw", "sec")))
		if strings.Contains(got, "127.0.0.1") {
			t.Errorf("%s binds loopback next to 0.0.0.0", name)
		}
	}
}
