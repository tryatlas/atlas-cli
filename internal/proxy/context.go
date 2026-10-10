package proxy

import "context"

type exchangeKey struct{}

func withExchange(ctx context.Context, ex *Exchange) context.Context {
	return context.WithValue(ctx, exchangeKey{}, ex)
}
