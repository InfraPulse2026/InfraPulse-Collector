# RouterOS response fixtures

Fixtures represent the JSON shapes and property names confirmed in official
MikroTik documentation. Values, names, addresses and serial numbers are invented
test data; these are not captures from real hardware and do not verify a model
or firmware version. All network tests use a local HTTPS server with a test CA.

- [REST API](https://manual.mikrotik.com/docs/developer-guides/rest-api/):
  menus, object/list responses, string values, `.proplist`, Basic authentication,
  TLS, `.id` record URLs, PATCH and `disabled` boolean representation.
- [Interface reference](https://manual.mikrotik.com/docs/cli-reference/interface/):
  `name`, `default-name`, `type`, `actual-mtu`, MAC and cumulative counters.
- [Resource](https://help.mikrotik.com/docs/spaces/ROS/pages/40992875/Resource):
  uptime, version, architecture, board, CPU and memory values.
- [RouterBOOT](https://help.mikrotik.com/docs/spaces/ROS/pages/136839241/RouterBOOT):
  model, serial number and current bootloader firmware.
- [Neighbor discovery](https://help.mikrotik.com/docs/spaces/ROS/pages/24805517/Neighbor+discovery):
  local interface evidence, discovery protocols, remote MAC/name and age;
  `interface-name` applies to CDP and is not fabricated for LLDP.
- [User](https://manual.mikrotik.com/docs/authentication-authorization-accounting/user/):
  local user group and explicitly granted/revoked policy evidence. The driver
  cannot infer rights for hidden users or RADIUS-only accounts.
- [Bridge ports](https://manual.mikrotik.com/docs/cli-reference/interface/bridge/port/),
  [Bonding](https://help.mikrotik.com/docs/spaces/ROS/pages/8323193/Bonding),
  [VLAN dependencies](https://help.mikrotik.com/docs/spaces/ROS/pages/19136718/Layer2+misconfiguration):
  dependency closure used for conservative protection before interface writes.

RouterOS-neighbor MAC is retained as raw evidence with unknown LLDP chassis
subtype. Configured observation TTL is an assumption, not an advertised LLDP TTL.
