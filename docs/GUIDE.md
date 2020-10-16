**Репозиторий преследует следующие цели:**

 * Объединить в себе пакеты для упрощения
создания сервисов на языке Go. 
 * Показать лучшие практики и подходы
для программистов, только пришедших в экосистему языка
 * Снизить порог вхождения, получить необходимые навыки, изучая 
предоставленный здесь код.
* Реализовать некоторые идеи из "Чистой архитектуры"

Начинающему программисту на Go(пусть даже опытному в другом ЯП) 
следует начать изучение со стандартной библиотеки языка.
К примеру, [https://pkg.go.dev/net/http?tab=doc](https://pkg.go.dev/net/http?tab=doc).
Она привьет изучающему хорошее чувство вкуса и предоставит 
достаточное понимание подходов, которые приняты в экосистеме 
языка.

Не пишите на Go, как привыкли на другом языке. Откажитесь от 
предыдущих паттернов и привычек. Пишите на Go, как на Go.

**Хороший стиль оформления кода.**
1. https://golang.org/doc/effective_go.html
1. https://github.com/golang/go/wiki/CodeReviewComments
1. https://sourcegraph.com/github.com/sourcegraph/about@454b55d5e44bea9a8e963525628f84e2bc57452f/-/blob/handbook/engineering/go_style_guide.md
1. https://talks.golang.org/2014/names.slide#1
1. https://blog.golang.org/package-names
1. https://rakyll.org/style-packages/

**Организация кода**
1. https://github.com/golang-standards/project-layout

**Как работать с зависимостями**
1. https://blog.golang.org/using-go-modules

**Почему работа с зависимостями в Go сделана не так, как в моем любимом языке?**
1. https://research.swtch.com/vgo-principles

**Качество кода**
1. Подключить gofmt, govet, goimport
1. Использовать линтер https://github.com/golangci/golangci-lint

**Обработка ошибок.**

1. https://blog.golang.org/go1.13-errors
1. https://blog.golang.org/error-handling-and-go
