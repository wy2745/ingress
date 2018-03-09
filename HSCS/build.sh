#!/bin/sh

NAME='HSCS'

latex $NAME
bibtex $NAME
latex $NAME
latex $NAME

dvipdf $NAME

#rm -f $NAME.{aux,bbl,blg,log,dvi}
