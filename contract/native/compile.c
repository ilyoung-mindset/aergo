/**
 * @file    compile.c
 * @copyright defined in aergo/LICENSE.txt
 */

#include "common.h"

#include "prep.h"
#include "parser.h"
#include "ast.h"
#include "strbuf.h"

#include "compile.h"

void
compile(char *path, flag_t flag)
{
    strbuf_t src;

    strbuf_init(&src);

    preprocess(path, flag, &src);
    parse(path, flag, &src);

    error_dump();
}

/* end of compile.c */