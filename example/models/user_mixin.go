package models

import (
	"context"
	"strings"

	"cloud.google.com/go/spanner"
	"github.com/MixinNetwork/bot-api-go-client"
	"github.com/MixinNetwork/ocean.one/example/config"
	"github.com/MixinNetwork/ocean.one/example/session"
	"github.com/MixinNetwork/ocean.one/example/uuid"
)

func (current *User) ConnectMixin(ctx context.Context, authorizationCode string) (*User, error) {
	accessToken, scope, err := bot.OAuthGetAccessToken(ctx, config.ClientId, config.ClientSecret, authorizationCode, "")
	if err != nil {
		return nil, session.ServerError(ctx, err)
	}
	if !strings.Contains(scope, "PROFILE:READ") {
		return nil, session.ForbiddenError(ctx)
	}

	me, err := bot.UserMe(ctx, accessToken)
	if err != nil {
		return nil, session.ServerError(ctx, err)
	}
	userId, err := uuid.FromString(me.UserId)
	if err != nil {
		return nil, session.ForbiddenError(ctx)
	}
	current.MixinId = spanner.NullString{Valid: true, StringVal: userId.String() + ":" + me.IdentityNumber}

	err = createConversation(ctx, "CONTACT", userId.String())
	if err != nil {
		return nil, session.ServerError(ctx, err)
	}
	err = session.Database(ctx).Apply(ctx, []*spanner.Mutation{
		spanner.Update("users", []string{"user_id", "mixin_id"}, []interface{}{current.UserId, current.MixinId}),
	}, "users", "UPDATE", "ConnectMixin")
	if err != nil {
		return nil, session.TransactionError(ctx, err)
	}
	return current, nil
}

func (current *User) MixinIdentityNumber() string {
	pair := strings.Split(current.MixinId.StringVal, ":")
	if len(pair) == 1 {
		return ""
	}
	return pair[1]
}

func (current *User) MixinUserId() string {
	pair := strings.Split(current.MixinId.StringVal, ":")
	if len(pair) == 1 {
		return ""
	}
	return pair[0]
}

func createConversation(ctx context.Context, category, participantId string) error {
	conversationId := bot.UniqueConversationId(config.ClientId, participantId)
	participant := bot.Participant{
		UserId: participantId,
		Role:   "",
	}
	participants := []bot.Participant{participant}
	_, err := bot.CreateConversation(ctx, category, conversationId, participants, config.ClientId, config.SessionId, config.SessionKey)
	return err
}
