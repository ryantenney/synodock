package dsm

import (
	"sort"
	"strconv"
)

// Network is a Container Manager network: a Docker network, with the containers on it.
type Network struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Subnet     string   `json:"subnet"`
	Gateway    string   `json:"gateway"`
	IPRange    string   `json:"iprange"`
	EnableIPv6 bool     `json:"enable_ipv6"`
	Containers []string `json:"containers"`
}

// Networks lists the networks, sorted by name.
func (c *Client) Networks() ([]Network, error) {
	var data struct {
		Network []Network `json:"network"`
	}
	if err := c.CallInto(&data, "SYNO.Docker.Network", "list", 1, nil); err != nil {
		return nil, err
	}
	nets := data.Network
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
	return nets, nil
}

// Network is the network called name.
func (c *Client) Network(name string) (*Network, error) {
	nets, err := c.Networks()
	if err != nil {
		return nil, err
	}
	for i := range nets {
		if nets[i].Name == name {
			return &nets[i], nil
		}
	}
	return nil, Errorf("no network named %q", name)
}

// NetworkCreate makes a network with a fixed IPv4 subnet, gateway and optional address
// range. IPv6 is off and masquerading stays on, as the UI's defaults have them.
func (c *Client) NetworkCreate(name, subnet, gateway, iprange string, disableMasquerade bool) error {
	params := Params{
		"name": JS(name), "ipv4_configs": JS("manual"), "subnet": JS(subnet), "gateway": JS(gateway),
		"ipv6_configs": JS("off"), "enable_ipv6": "false",
		"disable_masquerade": strconv.FormatBool(disableMasquerade),
	}
	if iprange != "" {
		params["iprange"] = JS(iprange)
	} else {
		params["iprange"] = JS("")
	}
	_, err := c.Call("SYNO.Docker.Network", "create", 1, params)
	return err
}

// NetworkRemove deletes networks by name. A network with containers on it is refused by
// Container Manager.
func (c *Client) NetworkRemove(names ...string) error {
	_, err := c.Call("SYNO.Docker.Network", "remove", 1, Params{"networks": JS(names)})
	return err
}
