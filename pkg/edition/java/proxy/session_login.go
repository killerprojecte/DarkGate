package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"go.minekube.com/common/minecraft/color"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/gate/pkg/edition/java/config"
	"go.minekube.com/gate/pkg/edition/java/internal/auth"
	"go.minekube.com/gate/pkg/edition/java/internal/profile"
	"go.minekube.com/gate/pkg/edition/java/proto/packet"
	"go.minekube.com/gate/pkg/edition/java/proto/state"
	"go.minekube.com/gate/pkg/edition/java/proto/version"
	"go.minekube.com/gate/pkg/event"
	"go.minekube.com/gate/pkg/gate/proto"
	"go.minekube.com/gate/pkg/runtime/logr"
	"go.minekube.com/gate/pkg/util/uuid"
	"net"
	"net/http"
)

type loginSessionHandler struct {
	conn    *minecraftConn
	inbound Inbound
	log     logr.Logger

	noOpSessionHandler

	// following fields are not goroutine-safe
	login  *packet.ServerLogin
	verify []byte
}

func newLoginSessionHandler(conn *minecraftConn, inbound Inbound) sessionHandler {
	return &loginSessionHandler{conn: conn, inbound: inbound, log: conn.log}
}

func (l *loginSessionHandler) handlePacket(p proto.Packet) {
	switch t := p.(type) {
	case *packet.ServerLogin:
		l.handleServerLogin(t)
	case *packet.EncryptionResponse:
		l.handleEncryptionResponse(t)
	default:
		_ = l.conn.close() // unknown packet, close connection
	}
}

func (l *loginSessionHandler) handleServerLogin(login *packet.ServerLogin) {
	l.login = login

	e := newPreLoginEvent(l.inbound, l.login.Username)
	l.event().Fire(e)

	if l.conn.Closed() {
		return // Player was disconnected
	}

	if e.Result() == DeniedPreLogin {
		_ = l.conn.closeWith(packet.DisconnectWithProtocol(e.Reason(), l.conn.Protocol()))
		return
	}

	if e.Result() != ForceOfflineModePreLogin && (e.Result() == ForceOnlineModePreLogin || l.config().OnlineMode) {
		// Online mode login, send encryption request
		request := l.generateEncryptionRequest()
		l.verify = make([]byte, len(request.VerifyToken))
		copy(l.verify, request.VerifyToken)
		_ = l.conn.WritePacket(request)

		// Wait for EncryptionResponse packet
		return
	}
	// Offline mode login
	l.initPlayer(profile.NewOffline(l.login.Username), false)
}

func (l *loginSessionHandler) generateEncryptionRequest() *packet.EncryptionRequest {
	verify := make([]byte, 4)
	_, _ = rand.Read(verify)
	return &packet.EncryptionRequest{
		PublicKey:   l.auth().PublicKey,
		VerifyToken: verify,
	}
}

var unableAuthWithMojang = &component.Text{
	Content: "Unable to authenticate you with Mojang.\nPlease try again!",
	S:       component.Style{Color: color.Red},
}

func (l *loginSessionHandler) handleEncryptionResponse(resp *packet.EncryptionResponse) {
	if l.login == nil || // No ServerLogin packet received yet
		len(l.verify) == 0 { // No EncryptionRequest packet sent yet
		_ = l.conn.close()
		return
	}

	authenticator := l.auth()
	decryptedVerifyToken, err := rsa.DecryptPKCS1v15(rand.Reader, authenticator.ServerKey, resp.VerifyToken)
	if err != nil {
		l.log.Error(err, "Could not decrypt verification token")
		_ = l.conn.close()
		return
	}
	if !bytes.Equal(l.verify, decryptedVerifyToken) {
		l.log.Error(err, "Unable to successfully decrypt the verification token.")
		_ = l.conn.close()
		return
	}

	decryptedSharedSecret, err := rsa.DecryptPKCS1v15(rand.Reader, authenticator.ServerKey, resp.SharedSecret)
	if err != nil {
		l.log.Error(err, "Could not decrypt verify token")
		_ = l.conn.close()
		return
	}

	// Enable encryption.
	// Once the client sends EncryptionResponse, encryption is enabled.
	if err = l.conn.enableEncryption(decryptedSharedSecret); err != nil {
		l.log.Error(err, "Error enabling encryption for connecting player")
		_ = l.conn.closeWith(packet.DisconnectWith(internalServerConnectionError))
		return
	}

	var userIp string
	getUserIP := func() string {
		if len(userIp) == 0 {
			userIp, _, _ = net.SplitHostPort(l.conn.RemoteAddr().String())
		}
		return userIp
	}

	var optionalUserIP string
	if l.config().ShouldPreventClientProxyConnections {
		optionalUserIP = getUserIP()
	}

	serverID := authenticator.GenerateServerID(decryptedSharedSecret)
	statusCode, body, err := authenticator.HasJoined(l.login.Username, optionalUserIP, serverID)
	if err != nil {
		if l.conn.closeWith(packet.DisconnectWith(unableAuthWithMojang)) == nil {
			l.log.Error(err, "Unable to authenticate player with Mojang")
		}
		return
	}
	if l.conn.Closed() {
		// The player disconnected after receiving the response.
		return
	}

	switch statusCode {
	case http.StatusOK:
		// All went well, initialize the session.
		gameProfile := new(profile.GameProfile)
		if err = json.Unmarshal(body, gameProfile); err != nil {
			if l.conn.closeWith(packet.DisconnectWith(unableAuthWithMojang)) == nil {
				l.log.Error(err, "Unable to unmarshal GameProfile from Mojang authentication response")
			}
			return
		}
		l.initPlayer(gameProfile, true)
	case http.StatusNoContent:
		// Apparently an offline-mode user logged onto this online-mode proxy.
		_ = l.conn.closeWith(packet.DisconnectWith(onlineModeOnly))
	default:
		// Something else went wrong
		l.log.Info("Got unexpected status error code whilst contacting Mojang to log in player",
			"statusCode", statusCode,
			"username", l.login.Username,
			"playerIP", getUserIP())
	}
}

var (
	onlineModeOnly = &component.Text{
		Content: `This server only accepts connections from online-mode clients.

Did you change your username? Sign out of Minecraft, sign back in, and try again.`,
		S: component.Style{Color: color.Red},
	}
)

func (l *loginSessionHandler) handleUnknownPacket(p *proto.PacketContext) {
	l.conn.close()
}

// Temporary english messages until localization support
var (
	alreadyConnected = &component.Text{
		Content: "You are already connected to this server!",
	}
	alreadyInProgress = &component.Text{
		Content: "You are already connecting to a server!",
	}
	noAvailableServers = &component.Text{
		Content: "No available server.", S: component.Style{Color: color.Red},
	}
	internalServerConnectionError = &component.Text{
		Content: "Internal server connection error",
	}
	//unexpectedDisconnect = &component.Text{
	//	Content: "Unexpectedly disconnected from remote server - crash?",
	//}
	movedToNewServer = &component.Text{
		Content: "The server you were on kicked you: ",
		S:       component.Style{Color: color.Red},
	}
)

func (l *loginSessionHandler) initPlayer(profile *profile.GameProfile, onlineMode bool) {
	// Some connection types may need to alter the game profile.
	profile = l.conn.Type().addGameProfileTokensIfRequired(profile,
		l.proxy().Config().Forwarding.Mode)

	profileRequest := NewGameProfileRequestEvent(l.inbound, *profile, onlineMode)
	l.proxy().event.Fire(profileRequest)
	if l.conn.Closed() {
		return // Player disconnected after authentication
	}
	gameProfile := profileRequest.GameProfile()

	// Initiate a regular connection and move over to it.
	player := newConnectedPlayer(l.conn, &gameProfile, l.inbound.VirtualHost(), onlineMode)
	if !l.proxy().canRegisterConnection(player) {
		player.Disconnect(alreadyConnected)
		return
	}

	l.log.Info("Player has connected, completing login", "player", player)

	// Setup permissions
	permSetup := &PermissionsSetupEvent{
		subject:     player,
		defaultFunc: player.permFunc,
	}
	player.proxy.event.Fire(permSetup)
	// Set the players permission function
	player.permFunc = permSetup.Func()

	if player.Active() {
		l.completeLoginProtocolPhaseAndInit(player)
	}
}

func (l *loginSessionHandler) completeLoginProtocolPhaseAndInit(player *connectedPlayer) {
	cfg := l.config()

	// Send compression threshold
	threshold := cfg.Compression.Threshold
	if threshold >= 0 && player.Protocol().GreaterEqual(version.Minecraft_1_8) {
		err := player.WritePacket(&packet.SetCompression{Threshold: threshold})
		if err != nil {
			player.close()
			return
		}
		if err := player.SetCompressionThreshold(threshold); err != nil {
			l.log.Error(err, "Error setting compression threshold")
			_ = player.closeWith(packet.DisconnectWith(internalServerConnectionError))
			return
		}
	}

	// Send login success
	playerID := player.ID()
	if cfg.Forwarding.Mode == config.NoneForwardingMode {
		playerID = uuid.OfflinePlayerUUID(player.Username())
	}
	if player.WritePacket(&packet.ServerLoginSuccess{
		UUID:     playerID,
		Username: player.Username(),
	}) != nil {
		return
	}

	player.setState(state.Play)
	loginEvent := &LoginEvent{player: player}
	l.event().Fire(loginEvent)

	if !player.Active() {
		l.event().Fire(&DisconnectEvent{
			player:      player,
			loginStatus: CanceledByUserBeforeCompleteLoginStatus,
		})
		return
	}

	if !loginEvent.Allowed() {
		player.Disconnect(loginEvent.Reason())
		return
	}

	if !l.proxy().registerConnection(player) {
		player.Disconnect(alreadyConnected)
		return
	}

	// Login is done now, just connect player to first server and
	// let InitialConnectSessionHandler do further work.
	player.setSessionHandler(newInitialConnectSessionHandler(player))
	l.event().Fire(&PostLoginEvent{player: player})
	l.connectToInitialServer(player)
}

func (l *loginSessionHandler) connectToInitialServer(player *connectedPlayer) {
	initialFromConfig := player.nextServerToTry(nil)
	chooseServer := &PlayerChooseInitialServerEvent{
		player:        player,
		initialServer: initialFromConfig,
	}
	l.event().Fire(chooseServer)
	if chooseServer.InitialServer() == nil {
		player.Disconnect(noAvailableServers) // Will call disconnected() in InitialConnectSessionHandler
		return
	}
	ctx, cancel := withConnectionTimeout(context.Background(), l.config())
	defer cancel()
	player.CreateConnectionRequest(chooseServer.InitialServer()).ConnectWithIndication(ctx)
}

func (l *loginSessionHandler) proxy() *Proxy {
	return l.conn.proxy
}

func (l *loginSessionHandler) event() event.Manager {
	return l.proxy().event
}

func (l *loginSessionHandler) config() *config.Config {
	return l.proxy().config
}

func (l *loginSessionHandler) auth() *auth.Authenticator {
	return l.proxy().authenticator
}
