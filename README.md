# Agente de Impressão — Gestor Escolar

> Código-fonte aberto do agente de impressão do [Gestor Escolar](https://app.pedagogicoonline.com.br)
> (ERP escolar privado). Agente local (Windows/Linux) que descobre impressoras e imprime jobs
> recebidos do backend via SumatraPDF (Windows) ou CUPS (Linux). Licença [MIT](LICENSE).

## Build

```bash
./build.sh
```

Requer Go 1.22+, Docker (ou Inno Setup/Wine local) para o instalador Windows, e SumatraPDF em disco
— ver `./build.sh --help`.

## Assinatura de código

Os `.exe` Windows hoje não são assinados. Projeto candidato ao certificado gratuito da
[SignPath Foundation](https://signpath.org) para software open source — aplicação em andamento.
Variáveis `CODESIGN_*`: ver `./build.sh --help`.

## Licença

[MIT](LICENSE)
