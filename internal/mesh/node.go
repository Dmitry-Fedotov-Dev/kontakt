package mesh

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"
)

// Version — версия протокола mesh в рукопожатии.
const Version = "kontakt-mesh/1"

// Stats — то, что узел знает о своей нагрузке (подаёт медиа-слой; в фазе 1 — нули).
type Stats struct {
	ActiveCalls int
	TotalBytes  uint64 // накопленный трафик узла в обе стороны: полоса по факту
}

// Config — настройки узла.
type Config struct {
	Role       Role
	Name       string // имя узла для людей (на графе); по умолчанию — начало NodeID
	Identity   *Identity
	Listen     string            // адрес для входящих mesh-соединений (TCP)
	Advertise  string            // как до нас достучаться (по умолчанию Listen)
	MasterAddr string            // Worker: где Master; пусто — без Master'а
	MasterPub  ed25519.PublicKey // Worker: закреплённый ключ Master'а
	JoinToken  string            // общий секрет допуска: Master проверяет, Worker доказывает владение
	Bootstrap  []string          // адреса доверенных Worker'ов для старта без Master'а (§11)
	Trusted    []NodeID          // локальный список доверенных узлов (§8)

	MaxPeers          int
	MaxHops           int
	ProbeInterval     time.Duration
	ProbeTimeout      time.Duration // сколько ждать ответа на пробу; дольше — потеря (по умолчанию 4×интервал)
	AdvertInterval    time.Duration
	AdvertTTL         time.Duration
	HeartbeatInterval time.Duration
	AdmissionTTL      time.Duration // Master: срок допуска
	RegistryTTL       time.Duration // Master

	CapacityMbps float64 // ёмкость канала, если оператор её знает (замер speedtest); 0 — неизвестна
	Reserve      float64 // доля канала, которую держим свободной (§25), по умолчанию 0.3
	Weights      Weights
	Thresholds   Thresholds
	Transport    Transport
	Stats        func() Stats

	// testDrop — доля ping/pong, которые узел «теряет» на ребре к peer (только тесты).
	testDrop func(peer NodeID) float64
}

func (c *Config) defaults() {
	if c.MaxPeers <= 0 {
		c.MaxPeers = 8
	}
	if c.MaxHops <= 0 {
		c.MaxHops = 4
	}
	c.ProbeInterval = orDefault(c.ProbeInterval, time.Second)
	c.ProbeTimeout = orDefault(c.ProbeTimeout, 4*c.ProbeInterval)
	c.AdvertInterval = orDefault(c.AdvertInterval, 5*time.Second)
	c.AdvertTTL = orDefault(c.AdvertTTL, 4*c.AdvertInterval)
	c.HeartbeatInterval = orDefault(c.HeartbeatInterval, 5*time.Second)
	c.AdmissionTTL = orDefault(c.AdmissionTTL, 24*time.Hour)
	c.RegistryTTL = orDefault(c.RegistryTTL, 3*c.HeartbeatInterval)
	if c.Reserve <= 0 {
		c.Reserve = 0.3
	}
	if c.Weights == (Weights{}) {
		c.Weights = DefaultWeights()
	}
	if c.Thresholds == (Thresholds{}) {
		c.Thresholds = DefaultThresholds()
	}
	if c.Transport == nil {
		c.Transport = TCPTransport{}
	}
	if c.Advertise == "" {
		c.Advertise = c.Listen
	}
	if c.Stats == nil {
		c.Stats = func() Stats { return Stats{} }
	}
}

// ---------- тела сообщений ----------

type helloBody struct {
	Role      Role       `json:"role"`
	Addr      string     `json:"addr"`
	Admission *Admission `json:"adm,omitempty"`
	Version   string     `json:"version"`
}

type registerBody struct {
	Role       Role       `json:"role"`
	Name       string     `json:"name,omitempty"`
	Addr       string     `json:"addr"`
	Proof      []byte     `json:"proof"` // HMAC(токен, NodeID|время): токен по сети не идёт
	ProofTS    int64      `json:"proof_ts"`
	Transports []PathKind `json:"transports"`
	Version    string     `json:"version"`
}

type admissionBody struct {
	Admission Admission  `json:"adm"`
	Peers     []PeerInfo `json:"peers"`
}

type peersBody struct {
	Peers []PeerInfo `json:"peers,omitempty"`
}

type probeBody struct {
	N uint64 `json:"n"`
}

type topoBody struct {
	Env  Envelope `json:"env"`  // объявление в исходной подписи источника
	Hops int      `json:"hops"` // вне подписи: меняется при пересылке
}

func joinProof(token string, id NodeID, ts int64) []byte {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(id))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(ts))
	m.Write(b[:])
	return m.Sum(nil)
}

// ---------- узел ----------

type peer struct {
	id        NodeID
	pub       ed25519.PublicKey
	role      Role
	addr      string
	conn      Conn
	initiator NodeID // кто набрал: при двойном соединении выживает набранное меньшим NodeID
	prober    *Prober
	since     time.Time
}

// Node — узел mesh любой роли.
type Node struct {
	cfg      Config
	ID       NodeID
	signer   *Signer
	verifier *Verifier
	advVer   *Verifier // для объявлений: без проверки повтора
	Trust    *Trust
	Topo     *Topology
	Registry *Registry // только у Master'а
	Router   Router
	meter    *Meter

	mu        sync.Mutex
	peers     map[NodeID]*peer
	dialing   map[string]bool
	admission *Admission
	masterUp  bool
	failover  map[NodeID]*Failover
	routes    map[NodeID][]Route
	advVer64  uint64
	stop      chan struct{}
	closed    bool
	ctrl      map[Conn]bool // Master: соединения регистрации Worker'ов
	closers   []func()
	wg        sync.WaitGroup
	routeFail int
}

func NewNode(cfg Config) (*Node, error) {
	cfg.defaults()
	if cfg.Identity == nil {
		return nil, errors.New("mesh: нужна идентичность узла")
	}
	n := &Node{cfg: cfg, ID: cfg.Identity.ID, signer: NewSigner(cfg.Identity), verifier: NewVerifier(),
		advVer: NewVerifier(), Trust: NewTrust(cfg.MasterPub), Topo: NewTopology(cfg.Identity.ID, cfg.MaxHops),
		Router: Router{W: cfg.Weights, Th: cfg.Thresholds}, meter: NewMeter(),
		peers: map[NodeID]*peer{}, dialing: map[string]bool{}, failover: map[NodeID]*Failover{}, routes: map[NodeID][]Route{}, ctrl: map[Conn]bool{}, stop: make(chan struct{})}
	n.Trust.AddTrusted(cfg.Trusted...)
	if cfg.Role == RoleMaster {
		n.Registry = NewRegistry(cfg.RegistryTTL)
	}
	return n, nil
}

// Start поднимает приём соединений и фоновые циклы.
func (n *Node) Start() error {
	if n.cfg.Listen != "" {
		l, err := ListenTCP(n.cfg.Listen, n.accept)
		if err != nil {
			return err
		}
		if n.cfg.Advertise == n.cfg.Listen || n.cfg.Advertise == "" {
			n.cfg.Advertise = l.Addr().String()
		}
		n.closers = append(n.closers, func() { l.Close() })
	}
	switch n.cfg.Role {
	case RoleMaster:
		n.loop(n.cfg.RegistryTTL/3, func() { n.Registry.Expire() })
	default:
		if n.cfg.MasterAddr != "" {
			n.wg.Add(1)
			go n.masterLoop()
		}
		for _, a := range n.cfg.Bootstrap {
			go n.dial(a)
		}
		n.loop(n.cfg.ProbeInterval, n.probeAll)
		n.loop(n.cfg.AdvertInterval, n.advertise)
		n.loop(n.cfg.AdvertInterval, func() { n.Topo.Expire(); n.refreshRoutes() })
	}
	return nil
}

// Addr — адрес, по которому узел принимает соединения.
func (n *Node) Addr() string { return n.cfg.Advertise }

// Close останавливает узел и рвёт его соединения.
func (n *Node) Close() {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	close(n.stop)
	for _, c := range n.closers {
		c()
	}
	n.mu.Lock()
	for _, p := range n.peers {
		p.conn.Close()
	}
	for c := range n.ctrl { // иначе читатели heartbeat'ов ждали бы вечно, а с ними и Wait
		c.Close()
	}
	n.mu.Unlock()
	n.wg.Wait()
}

func (n *Node) loop(every time.Duration, f func()) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-n.stop:
				return
			case <-t.C:
				f()
			}
		}
	}()
}

func (n *Node) send(c Conn, typ string, body any) error {
	e, err := n.signer.Seal(typ, body)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(e)
	return c.Send(b)
}

func (n *Node) recv(c Conn, timeout time.Duration) (*Envelope, error) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { b, err := c.Receive(); ch <- res{b, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		var e Envelope
		if err := json.Unmarshal(r.b, &e); err != nil {
			return nil, err
		}
		return &e, nil
	case <-time.After(timeout):
		c.Close()
		return nil, errors.New("тайм-аут рукопожатия")
	}
}

// ---------- рукопожатие ----------

func (n *Node) hello() helloBody {
	n.mu.Lock()
	defer n.mu.Unlock()
	return helloBody{Role: n.cfg.Role, Addr: n.cfg.Advertise, Admission: n.admission, Version: Version}
}

// accept — входящее соединение: первым сообщением ждём hello или register.
func (n *Node) accept(c Conn) {
	e, err := n.recv(c, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	if err := n.verifier.Verify(e); err != nil {
		log.Printf("mesh: %s: отвергнуто: %v", c.RemoteAddr(), err)
		c.Close()
		return
	}
	switch {
	case e.Type == MsgRegister && n.cfg.Role == RoleMaster:
		n.onRegister(c, e)
	case e.Type == MsgHello && n.cfg.Role != RoleMaster:
		var h helloBody
		if e.Decode(&h) != nil {
			c.Close()
			return
		}
		if err := n.send(c, MsgHello, n.hello()); err != nil {
			c.Close()
			return
		}
		n.admit(c, e, h, e.From)
	default:
		c.Close()
	}
}

// dial — исходящее соединение к соседу.
func (n *Node) dial(addr string) {
	if addr == "" || addr == n.cfg.Advertise {
		return
	}
	n.mu.Lock()
	if n.dialing[addr] {
		n.mu.Unlock()
		return
	}
	n.dialing[addr] = true
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.dialing, addr); n.mu.Unlock() }()

	c, err := n.cfg.Transport.Dial(addr)
	if err != nil {
		return
	}
	if err := n.send(c, MsgHello, n.hello()); err != nil {
		c.Close()
		return
	}
	e, err := n.recv(c, 5*time.Second)
	if err != nil || e.Type != MsgHello || n.verifier.Verify(e) != nil {
		c.Close()
		return
	}
	var h helloBody
	if e.Decode(&h) != nil {
		c.Close()
		return
	}
	n.admit(c, e, h, n.ID)
}

// admit — после обмена hello: решаем по доверию, оставляем или рвём.
func (n *Node) admit(c Conn, e *Envelope, h helloBody, initiator NodeID) {
	id := e.From
	if id == n.ID {
		c.Close()
		return
	}
	if st := n.Trust.Evaluate(id, h.Admission, time.Now()); st < Authorized {
		log.Printf("mesh: %s (%s) не авторизован (%s): соединение не принято", id, c.RemoteAddr(), st)
		c.Close()
		return
	}
	p := &peer{id: id, pub: e.Pub, role: h.Role, addr: h.Addr, conn: c, initiator: initiator,
		prober: NewProber(20, n.cfg.ProbeTimeout), since: time.Now()}

	n.mu.Lock()
	if old := n.peers[id]; old != nil {
		// Двойное соединение: оставляем набранное меньшим NodeID — обе стороны решат одинаково.
		winner := n.ID
		if id < winner {
			winner = id
		}
		if old.initiator == winner || p.initiator != winner {
			n.mu.Unlock()
			c.Close()
			return
		}
		old.conn.Close()
	}
	if n.closed || len(n.peers) >= n.cfg.MaxPeers && n.peers[id] == nil {
		n.mu.Unlock()
		c.Close()
		return
	}
	n.peers[id] = p
	n.Trust.SetConnected(id, true) // под тем же замком: сосед не виден в peers без CONNECTED
	n.wg.Add(1)                    // под замком вместе с closed: Close не может начать Wait между проверкой и Add
	n.mu.Unlock()
	go n.readPeer(p)
	n.advertise() // о новом ребре соседи узнают сразу, а не через интервал
}

func (n *Node) dropPeer(p *peer) {
	n.mu.Lock()
	cur := n.peers[p.id]
	if cur == p {
		delete(n.peers, p.id)
		n.Trust.SetConnected(p.id, false)
	}
	n.mu.Unlock()
	p.conn.Close()
	if cur == p {
		n.advertise()
		n.refreshRoutes()
	}
}

func (n *Node) readPeer(p *peer) {
	defer n.wg.Done()
	defer n.dropPeer(p)
	for {
		b, err := p.conn.Receive()
		if err != nil {
			return
		}
		var e Envelope
		if json.Unmarshal(b, &e) != nil {
			return
		}
		if e.Type == MsgTopology {
			n.onTopology(p, &e)
			continue
		}
		if e.From != p.id || n.verifier.Verify(&e) != nil {
			continue
		}
		switch e.Type {
		case MsgPing:
			var pb probeBody
			if e.Decode(&pb) == nil && !n.dropped(p.id) {
				n.send(p.conn, MsgPong, pb)
			}
		case MsgPong:
			var pb probeBody
			if e.Decode(&pb) == nil && !n.dropped(p.id) {
				p.prober.Pong(pb.N)
			}
		case MsgPeers:
			var pb peersBody
			if e.Decode(&pb) != nil {
				continue
			}
			if len(pb.Peers) == 0 { // запрос: отдаём не больше MaxPeers своих соседей
				n.send(p.conn, MsgPeers, peersBody{Peers: n.peerInfos(p.id)})
			} else {
				n.connectPeers(pb.Peers)
			}
		}
	}
}

func (n *Node) dropped(id NodeID) bool {
	if n.cfg.testDrop == nil {
		return false
	}
	frac := n.cfg.testDrop(id)
	return frac > 0 && float64(time.Now().UnixNano()%1000)/1000 < frac
}

func (n *Node) peerInfos(except NodeID) []PeerInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []PeerInfo
	for id, p := range n.peers {
		if id != except && p.addr != "" && p.role != RoleMaster {
			out = append(out, PeerInfo{ID: id, Pub: p.pub, Role: p.role, Addr: p.addr, Health: Healthy})
		}
		if len(out) >= n.cfg.MaxPeers {
			break
		}
	}
	return out
}

func (n *Node) connectPeers(ps []PeerInfo) {
	for _, pi := range ps {
		if pi.ID == n.ID || pi.Role == RoleMaster || pi.Health == Offline {
			continue
		}
		n.Trust.MarkKnown(pi.ID)
		n.mu.Lock()
		_, have := n.peers[pi.ID]
		full := len(n.peers) >= n.cfg.MaxPeers
		n.mu.Unlock()
		if !have && !full {
			go n.dial(pi.Addr)
		}
	}
}

// ---------- Master ----------

func (n *Node) onRegister(c Conn, e *Envelope) {
	var rb registerBody
	if e.Decode(&rb) != nil {
		c.Close()
		return
	}
	if d := time.Since(time.Unix(0, rb.ProofTS)); d > time.Minute || d < -time.Minute ||
		!hmac.Equal(rb.Proof, joinProof(n.cfg.JoinToken, e.From, rb.ProofTS)) {
		log.Printf("mesh: %s: регистрация без верного токена", e.From)
		c.Close()
		return
	}
	info := PeerInfo{ID: e.From, Name: rb.Name, Pub: e.Pub, Role: rb.Role, Addr: rb.Addr, Health: Healthy}
	n.Registry.Upsert(info, rb.Transports, rb.Version)
	adm := IssueAdmission(n.cfg.Identity, e.From, e.Pub, rb.Role, n.cfg.AdmissionTTL)
	if n.send(c, MsgAdmission, admissionBody{Admission: adm, Peers: n.Registry.Peers(e.From, n.cfg.MaxPeers)}) != nil {
		c.Close()
		return
	}
	// дальше на этом соединении — heartbeat'ы и запросы соседей
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		c.Close()
		return
	}
	n.ctrl[c] = true
	n.wg.Add(1)
	n.mu.Unlock()
	go func() {
		defer n.wg.Done()
		defer func() { n.mu.Lock(); delete(n.ctrl, c); n.mu.Unlock(); c.Close() }()
		for {
			b, err := c.Receive()
			if err != nil {
				return
			}
			var m Envelope
			if json.Unmarshal(b, &m) != nil || m.From != e.From || n.verifier.Verify(&m) != nil {
				continue
			}
			switch m.Type {
			case MsgHeartbeat:
				var hb Heartbeat
				if m.Decode(&hb) == nil {
					n.Registry.Beat(m.From, hb)
				}
			case MsgPeers:
				n.send(c, MsgPeers, peersBody{Peers: n.Registry.Peers(m.From, n.cfg.MaxPeers)})
			}
		}
	}()
}

// ---------- Worker ↔ Master ----------

// masterLoop держит связь с Master'ом: регистрация, допуск, соседи, heartbeat.
// Master пропал — ничего не рвём: соседи, маршруты и допуск остаются (§6).
func (n *Node) masterLoop() {
	defer n.wg.Done()
	backoff := time.Second
	for {
		err := n.masterSession()
		n.mu.Lock()
		n.masterUp = false
		n.mu.Unlock()
		if err != nil {
			log.Printf("mesh: Master %s: %v (повтор через %v)", n.cfg.MasterAddr, err, backoff)
		}
		select {
		case <-n.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (n *Node) masterSession() error {
	c, err := n.cfg.Transport.Dial(n.cfg.MasterAddr)
	if err != nil {
		return err
	}
	defer c.Close()
	ts := time.Now().UnixNano()
	if err := n.send(c, MsgRegister, registerBody{Role: n.cfg.Role, Name: n.cfg.Name, Addr: n.cfg.Advertise,
		Proof: joinProof(n.cfg.JoinToken, n.ID, ts), ProofTS: ts,
		Transports: []PathKind{n.cfg.Transport.Kind()}, Version: Version}); err != nil {
		return err
	}
	e, err := n.recv(c, 10*time.Second)
	if err != nil {
		return err
	}
	if !e.Pub.Equal(n.cfg.MasterPub) || n.verifier.Verify(e) != nil || e.Type != MsgAdmission {
		return errors.New("ответ не от нашего Master'а")
	}
	var ab admissionBody
	if err := e.Decode(&ab); err != nil {
		return err
	}
	if err := ab.Admission.Check(n.cfg.MasterPub, time.Now()); err != nil || ab.Admission.Node != n.ID {
		return errors.New("допуск не принят")
	}
	n.mu.Lock()
	n.admission, n.masterUp = &ab.Admission, true
	n.mu.Unlock()
	n.connectPeers(ab.Peers)

	errc := make(chan error, 1)
	go func() {
		for {
			b, err := c.Receive()
			if err != nil {
				errc <- err
				return
			}
			var m Envelope
			if json.Unmarshal(b, &m) != nil || !m.Pub.Equal(n.cfg.MasterPub) || n.verifier.Verify(&m) != nil {
				continue
			}
			var pb peersBody
			if m.Type == MsgPeers && m.Decode(&pb) == nil {
				n.connectPeers(pb.Peers)
			}
		}
	}()
	t := time.NewTicker(n.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return nil
		case err := <-errc:
			return err
		case <-t.C:
			if err := n.send(c, MsgHeartbeat, n.heartbeat()); err != nil {
				return err
			}
			n.mu.Lock()
			short := len(n.peers) < n.cfg.MaxPeers
			n.mu.Unlock()
			if short {
				n.send(c, MsgPeers, peersBody{})
			}
		}
	}
}

// MasterUp — есть ли сейчас связь с Master'ом.
func (n *Node) MasterUp() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.masterUp
}

// ---------- замеры, ёмкость, объявления ----------

func (n *Node) probeAll() {
	n.mu.Lock()
	ps := make([]*peer, 0, len(n.peers))
	for _, p := range n.peers {
		ps = append(ps, p)
	}
	n.mu.Unlock()
	for _, p := range ps {
		if !n.dropped(p.id) {
			n.send(p.conn, MsgPing, probeBody{N: p.prober.Next()})
		} else {
			p.prober.Next() // «потерянная» проба тоже проба
		}
	}
	st := n.cfg.Stats()
	n.meter.Sample(st.TotalBytes)
}

// Capacity — полоса и звонки узла по факту.
func (n *Node) Capacity() (NetworkCapacity, CallCapacity) {
	st := n.cfg.Stats()
	n.mu.Lock()
	sessions := len(n.peers)
	n.mu.Unlock()
	nc := Capacity(n.meter.Bps(), n.cfg.CapacityMbps, n.cfg.Reserve, sessions)
	return nc, Calls(nc, st.ActiveCalls, n.cfg.Reserve)
}

func (n *Node) health(nc NetworkCapacity) Health {
	if nc.Utilization >= n.cfg.Thresholds.BandwidthWarning {
		return Degraded
	}
	return Healthy
}

func (n *Node) heartbeat() Heartbeat {
	nc, cc := n.Capacity()
	cpu, mem := processUsage()
	return Heartbeat{Health: n.health(nc), CPU: cpu, MemoryMB: mem, ActiveCalls: cc.ActiveCalls,
		BandwidthBps: nc.ObservedMbps * 1e6, Utilization: nc.Utilization, CallsFree: cc.Calls, Version: Version,
		Links: n.Links()}
}

// Links — рёбра к соседям с замерами.
func (n *Node) Links() []AdvertLink {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]AdvertLink, 0, len(n.peers))
	for id, p := range n.peers {
		if p.role == RoleMaster {
			continue
		}
		m := p.prober.Metrics()
		m.Uptime = time.Since(p.since)
		out = append(out, AdvertLink{To: id, Kind: p.conn.Kind(), Metrics: m})
	}
	return out
}

func (n *Node) advertise() {
	select {
	case <-n.stop:
		return
	default:
	}
	nc, cc := n.Capacity()
	n.mu.Lock()
	n.advVer64++
	ver := n.advVer64
	n.mu.Unlock()
	a := Advert{Origin: n.ID, Version: ver, Issued: time.Now().UnixNano(), TTL: int64(n.cfg.AdvertTTL),
		Role: n.cfg.Role, Name: n.cfg.Name, Health: n.health(nc), Utilization: nc.Utilization, CallsFree: cc.Calls, Links: n.Links()}
	n.Topo.Apply(&a)
	e, err := n.signer.Seal(MsgTopology, a)
	if err != nil {
		return
	}
	n.flood(topoBody{Env: *e, Hops: 1}, "")
}

func (n *Node) flood(tb topoBody, except NodeID) {
	n.mu.Lock()
	ps := make([]*peer, 0, len(n.peers))
	for id, p := range n.peers {
		if id != except {
			ps = append(ps, p)
		}
	}
	n.mu.Unlock()
	for _, p := range ps {
		n.send(p.conn, MsgTopology, tb)
	}
}

// onTopology — объявление от соседа (своё или пересланное). Подпись — источника.
func (n *Node) onTopology(from *peer, wrapper *Envelope) {
	if wrapper.From != from.id || n.verifier.VerifySig(wrapper) != nil {
		return
	}
	var tb topoBody
	if wrapper.Decode(&tb) != nil || tb.Env.Type != MsgTopology {
		return
	}
	// Окно по времени для объявлений — их TTL, а не окно управляющих сообщений.
	if IDFromPublicKey(tb.Env.Pub) != tb.Env.From || !ed25519.Verify(tb.Env.Pub, tb.Env.signedBytes(), tb.Env.Sig) {
		return
	}
	var a Advert
	if tb.Env.Decode(&a) != nil || a.Origin != tb.Env.From {
		return
	}
	a.Hops = tb.Hops
	if n.Topo.Apply(&a) {
		n.flood(topoBody{Env: tb.Env, Hops: tb.Hops + 1}, from.id)
	}
}

// ---------- маршруты ----------

// Route — маршрут к dst: текущий и альтернативы. С первого вызова узел следит за dst
// и пересчитывает маршрут в фоне при каждом обновлении топологии: failover идёт без
// запроса. Сам вызов состояние не двигает — иначе частый опрос «проматывал» бы
// испытательный срок восстановившегося маршрута.
func (n *Node) Route(dst NodeID) (*Route, []Route) {
	n.mu.Lock()
	f, watched := n.failover[dst], n.routes[dst]
	n.mu.Unlock()
	if f == nil {
		n.updateRoute(dst)
		n.mu.Lock()
		f, watched = n.failover[dst], n.routes[dst]
		n.mu.Unlock()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return f.Active(), f.Alternatives(watched)
}

func (n *Node) updateRoute(dst NodeID) {
	routes := n.Router.Routes(n.adverts(), n.ID, dst, 3)
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.failover[dst]
	if f == nil {
		f = NewFailover(3, 0.2)
		n.failover[dst] = f
	}
	before := f.Failovers
	f.Update(routes)
	n.routeFail += f.Failovers - before
	n.routes[dst] = routes
}

func (n *Node) refreshRoutes() {
	n.mu.Lock()
	dsts := make([]NodeID, 0, len(n.failover))
	for d := range n.failover {
		dsts = append(dsts, d)
	}
	n.mu.Unlock()
	for _, d := range dsts {
		n.updateRoute(d)
	}
}

// RouteFailovers — сколько раз основной маршрут отказывал и заменялся (§38).
func (n *Node) RouteFailovers() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.routeFail
}

// adverts — граф: чужие объявления плюс собственное свежее (свои рёбра — из замеров
// прямо сейчас, а не из последнего объявления).
func (n *Node) adverts() map[NodeID]Advert {
	ads := n.Topo.Snapshot()
	nc, cc := n.Capacity()
	ads[n.ID] = Advert{Origin: n.ID, Role: n.cfg.Role, Name: n.cfg.Name, Health: n.health(nc), Utilization: nc.Utilization,
		CallsFree: cc.Calls, Links: n.Links()}
	return ads
}

// Peers — соседи с замерами.
func (n *Node) Peers() map[NodeID]LinkMetrics {
	out := map[NodeID]LinkMetrics{}
	for _, l := range n.Links() {
		out[l.To] = l.Metrics
	}
	return out
}

// Status — состояние узла для админки (/mesh/status).
type Status struct {
	ID        NodeID                 `json:"id"`
	Role      Role                   `json:"role"`
	Addr      string                 `json:"addr"`
	MasterUp  bool                   `json:"master_up"`
	Peers     []PeerStatus           `json:"peers"`
	Capacity  NetworkCapacity        `json:"capacity"`
	Calls     CallCapacity           `json:"calls"`
	Topology  []NodeID               `json:"topology"`
	Routes    map[NodeID]RouteStatus `json:"routes"`
	Failovers int                    `json:"route_failovers"`
	Registry  []Entry                `json:"registry,omitempty"`
}

type PeerStatus struct {
	ID      NodeID      `json:"id"`
	State   string      `json:"trust"`
	Kind    PathKind    `json:"kind"`
	Metrics LinkMetrics `json:"metrics"`
}

type RouteStatus struct {
	Active       *Route  `json:"active"`
	Alternatives []Route `json:"alternatives"`
}

func (n *Node) Status() Status {
	nc, cc := n.Capacity()
	st := Status{ID: n.ID, Role: n.cfg.Role, Addr: n.cfg.Advertise, MasterUp: n.MasterUp(), Capacity: nc, Calls: cc,
		Topology: n.Topo.Nodes(), Routes: map[NodeID]RouteStatus{}, Failovers: n.RouteFailovers()}
	for _, l := range n.Links() {
		st.Peers = append(st.Peers, PeerStatus{ID: l.To, State: n.Trust.State(l.To).String(), Kind: l.Kind, Metrics: l.Metrics})
	}
	n.mu.Lock()
	dsts := make([]NodeID, 0, len(n.failover))
	for d := range n.failover {
		dsts = append(dsts, d)
	}
	n.mu.Unlock()
	for _, d := range dsts {
		a, alts := n.Route(d)
		st.Routes[d] = RouteStatus{Active: a, Alternatives: alts}
	}
	if n.Registry != nil {
		st.Registry = n.Registry.Snapshot()
	}
	return st
}
